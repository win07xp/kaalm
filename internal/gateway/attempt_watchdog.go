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
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// errUpstreamIdle is the cancel cause of an upstream attempt that sent
// nothing for one UpstreamTimeout: no response headers, or no body bytes
// since the last read.
var errUpstreamIdle = errors.New("upstream sent no data within the upstream timeout")

// attemptWatchdog bounds one upstream attempt by inactivity. It cancels the
// attempt's context with errUpstreamIdle when no progress is reported for
// one bound. The caller reports progress with kick: once when the response
// headers arrive, then on every body read that returns bytes. A long stream
// that keeps producing bytes runs to completion. The attempt also follows
// the caller's cancel until detach; after detach, only the idle bound and
// release end it.
type attemptWatchdog struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	bound  time.Duration

	mu         sync.Mutex
	detached   bool
	stopFollow func() bool
}

// newAttemptWatchdog derives the attempt context from parent's values, arms
// the timer, and passes parent's cancel on until detach. The caller must
// call release (directly, or by closing the body returned by watch) on
// every path.
func newAttemptWatchdog(parent context.Context, bound time.Duration) (context.Context, *attemptWatchdog) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	w := &attemptWatchdog{ctx: ctx, cancel: cancel, bound: bound}
	w.stopFollow = context.AfterFunc(parent, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.detached {
			cancel(context.Cause(parent))
		}
	})
	w.timer = time.AfterFunc(bound, func() { cancel(errUpstreamIdle) })
	return ctx, w
}

// detach stops passing the caller's cancel on to the attempt. A cancel
// already passed on stays.
func (w *attemptWatchdog) detach() {
	w.mu.Lock()
	w.detached = true
	w.mu.Unlock()
	w.stopFollow()
}

// kick restarts the bound. It is a no-op once the context is done.
func (w *attemptWatchdog) kick() {
	if w.ctx.Err() == nil {
		w.timer.Reset(w.bound)
	}
}

// release stops the timer and cancels the attempt context. It is idempotent
// and never reports the idle cause.
func (w *attemptWatchdog) release() {
	w.stopFollow()
	w.timer.Stop()
	w.cancel(context.Canceled)
}

// idle reports whether the watchdog ended the attempt.
func (w *attemptWatchdog) idle() bool {
	return errors.Is(context.Cause(w.ctx), errUpstreamIdle)
}

// watch wraps an upstream response body: each read that returns bytes
// restarts the bound, a read that ends the body after the bound passed wraps
// errUpstreamIdle, and Close releases the watchdog.
func (w *attemptWatchdog) watch(body io.ReadCloser) io.ReadCloser {
	return &watchedBody{ReadCloser: body, dog: w}
}

type watchedBody struct {
	io.ReadCloser
	dog *attemptWatchdog
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.dog.kick()
	}
	// Once the bound has passed, any end of the body is the stall, io.EOF
	// included: an upstream can answer the cancel by finishing its response
	// cleanly, and a clean end here would make a truncated stream look
	// complete.
	switch {
	case err == nil || !b.dog.idle():
	case errors.Is(err, errUpstreamIdle):
		// The transport reports the cancel cause itself.
	case errors.Is(err, io.EOF):
		err = errUpstreamIdle
	default:
		err = fmt.Errorf("%w: %w", errUpstreamIdle, err)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.ReadCloser.Close()
	b.dog.release()
	return err
}
