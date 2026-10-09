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
	"io"
	"net/http"
	"sync"
)

// scanBufSize is the start buffer of a relay's line scanner. A Scanner's
// line cap is the larger of its configured maximum and its buffer's
// capacity, so the pooled buffers keep exactly this capacity.
const scanBufSize = 64 * 1024

// scanBufPool reuses the scanner start buffers of the stream relays and
// the tools/list relay: one per stream was a quarter of the gateway's
// allocated bytes under streaming load. A Scanner that outgrows its buffer
// allocates its own; only the original goes back to the pool.
var scanBufPool = sync.Pool{New: func() any {
	b := make([]byte, scanBufSize)
	return &b
}}

// getScanBuf takes a start buffer; the caller passes (*buf)[:0] to
// Scanner.Buffer and returns buf with putScanBuf once no token is in use.
func getScanBuf() *[]byte { return scanBufPool.Get().(*[]byte) }

func putScanBuf(buf *[]byte) { scanBufPool.Put(buf) }

// eventFlusher is the upstream reader of a stream relay. It flushes the
// caller's response just before the relay waits on the upstream for more
// data, and only when an event ended since the last flush. A bufio.Scanner
// reads only when no complete line is left in its buffer, so events that
// arrive in one upstream read leave in one write, and a complete event is
// sent before the relay waits for the next one. Flushing per line instead
// costs a network write (and a TLS record) per line, three for each
// Anthropic event.
type eventFlusher struct {
	r io.Reader
	f http.Flusher // nil when the writer cannot flush
	// pending is set when the relay writes the blank line that ends an
	// event; written when it writes anything. flush clears both.
	pending, written bool
}

// Read flushes a pending event, then reads from the upstream. Errors pass
// through unchanged.
func (e *eventFlusher) Read(p []byte) (int, error) {
	if e.pending {
		e.flush()
	}
	return e.r.Read(p)
}

// wrote records that the relay wrote line (without its newline); a blank
// line ends an event.
func (e *eventFlusher) wrote(line []byte) {
	e.written = true
	if len(line) == 0 {
		e.pending = true
	}
}

// flush sends what the relay wrote to the caller.
func (e *eventFlusher) flush() {
	if e.f != nil {
		e.f.Flush()
	}
	e.pending, e.written = false, false
}

// end flushes anything written since the last flush, when the relay stops
// reading: the last event must not wait for the handler to return.
func (e *eventFlusher) end() {
	if e.written {
		e.flush()
	}
}
