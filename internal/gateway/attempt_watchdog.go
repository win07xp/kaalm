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
// that keeps producing bytes runs to completion.
type attemptWatchdog struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	bound  time.Duration
}

// newAttemptWatchdog derives the attempt context from parent and arms the
// timer. The caller must call release (directly, or by closing the body
// returned by watch) on every path.
func newAttemptWatchdog(parent context.Context, bound time.Duration) (context.Context, *attemptWatchdog) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &attemptWatchdog{ctx: ctx, cancel: cancel, bound: bound}
	w.timer = time.AfterFunc(bound, func() { cancel(errUpstreamIdle) })
	return ctx, w
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
