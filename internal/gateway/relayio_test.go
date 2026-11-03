/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"testing/iotest"
)

func TestEventFlusher(t *testing.T) {
	t.Run("no flush without an ended event", func(t *testing.T) {
		w := newCountingWriter()
		ef := &eventFlusher{r: &chunkReader{chunks: [][]byte{[]byte("a"), []byte("b")}}, f: w}
		buf := make([]byte, 8)
		_, _ = ef.Read(buf)
		ef.wrote([]byte("data: x"))
		_, _ = ef.Read(buf)
		if w.flushes != 0 {
			t.Errorf("flushes = %d, want 0 before an event ends", w.flushes)
		}
		ef.end()
		if w.flushes != 1 {
			t.Errorf("flushes = %d after end, want 1 for the unflushed line", w.flushes)
		}
		ef.end()
		if w.flushes != 1 {
			t.Errorf("flushes = %d after a second end, want no flush with nothing written", w.flushes)
		}
	})
	t.Run("one flush before the next read after an event ends", func(t *testing.T) {
		w := newCountingWriter()
		ef := &eventFlusher{r: &chunkReader{chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")}}, f: w}
		buf := make([]byte, 8)
		ef.wrote([]byte("data: x"))
		ef.wrote(nil)
		if w.flushes != 0 {
			t.Fatalf("flushed at the blank line; want the flush deferred to the next read")
		}
		_, _ = ef.Read(buf)
		_, _ = ef.Read(buf)
		if w.flushes != 1 {
			t.Errorf("flushes = %d, want 1", w.flushes)
		}
		ef.end()
		if w.flushes != 1 {
			t.Errorf("flushes = %d after end, want nothing left to flush", w.flushes)
		}
	})
	t.Run("read errors pass through", func(t *testing.T) {
		ef := &eventFlusher{r: iotest.ErrReader(errUpstreamIdle)}
		ef.wrote(nil)
		if _, err := ef.Read(make([]byte, 8)); !errors.Is(err, errUpstreamIdle) {
			t.Errorf("err = %v, want errUpstreamIdle", err)
		}
	})
}

// sizeRecordingReader serves data in reads of at most maxRead bytes and
// records the buffer size of every read.
type sizeRecordingReader struct {
	data    []byte
	maxRead int
	sizes   []int
}

func (r *sizeRecordingReader) Read(p []byte) (int, error) {
	r.sizes = append(r.sizes, len(p))
	n := copy(p[:min(len(p), r.maxRead)], r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestRelayBody_BufferFollowsTheAnswer(t *testing.T) {
	answer := func(size int) []byte {
		const tail = `"}],"usage":{"prompt_tokens":6,"completion_tokens":1}}`
		const head = `{"choices":[{"text":"`
		return []byte(head + strings.Repeat("a", size-len(head)-len(tail)) + tail)
	}
	for _, c := range []struct {
		name        string
		size        int
		maxRead     int
		wantBuffers []int
	}{
		{"an answer under the small buffer", 512, 1 << 30, []int{relayBufSize}},
		{"an answer that fills the small buffer on its last read", relayBufSize, 1 << 30, []int{relayBufSize}},
		{"small reads of a larger answer", 3 * relayBufSize / 2, 1024,
			[]int{relayBufSize, relayBufSize, relayBufSize, relayBufSize, relayBufSize, relayBufSize}},
		{"a read that fills the small buffer", 2*relayBufSize + scanBufSize, 1 << 30,
			[]int{relayBufSize, scanBufSize, scanBufSize}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := answer(c.size)
			body := &sizeRecordingReader{data: data, maxRead: c.maxRead}
			var sink bytes.Buffer
			reader := newOpenAIUsageReader()
			readErr, writeErr := relayBody(body, &sink, reader)
			if readErr != nil || writeErr != nil {
				t.Fatalf("relayBody: read %v, write %v", readErr, writeErr)
			}
			if !bytes.Equal(sink.Bytes(), data) {
				t.Errorf("relayed %d bytes that differ from the %d sent", sink.Len(), len(data))
			}
			if reader.usage.InputTokens != 6 || reader.usage.OutputTokens != 1 {
				t.Errorf("usage %+v, want 6 and 1", reader.usage)
			}
			if !slices.Equal(body.sizes, c.wantBuffers) {
				t.Errorf("read buffers %v, want %v", body.sizes, c.wantBuffers)
			}
		})
	}
}
