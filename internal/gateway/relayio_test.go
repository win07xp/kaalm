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
	"errors"
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
