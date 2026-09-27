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
	"io"
	"strings"
	"testing"
	"time"
)

func TestAttemptWatchdog_FiresWithTheIdleCause(t *testing.T) {
	ctx, dog := newAttemptWatchdog(context.Background(), 30*time.Millisecond)
	defer dog.release()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errUpstreamIdle) {
		t.Errorf("cause = %v, want errUpstreamIdle", cause)
	}
}

func TestAttemptWatchdog_ActivityKeepsTheAttemptAlive(t *testing.T) {
	ctx, dog := newAttemptWatchdog(context.Background(), 60*time.Millisecond)
	defer dog.release()
	for i := 0; i < 10; i++ { // 200ms total, well past one 60ms bound
		time.Sleep(20 * time.Millisecond)
		dog.kick()
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("a kicked watchdog cancelled the attempt: %v", context.Cause(ctx))
	}
}

func TestAttemptWatchdog_ReleaseCancelsWithoutTheIdleCause(t *testing.T) {
	ctx, dog := newAttemptWatchdog(context.Background(), time.Hour)
	dog.release()
	if ctx.Err() == nil {
		t.Fatal("release left the attempt context live")
	}
	if errors.Is(context.Cause(ctx), errUpstreamIdle) {
		t.Error("release must not report the idle cause")
	}
	dog.release() // idempotent
}

// stallReader returns its data, then blocks until ctx is done and fails the
// way a transport read on a cancelled request does.
type stallReader struct {
	ctx    context.Context
	data   io.Reader
	closed bool
}

func (r *stallReader) Read(p []byte) (int, error) {
	if n, err := r.data.Read(p); n > 0 || !errors.Is(err, io.EOF) {
		return n, err
	}
	<-r.ctx.Done()
	return 0, context.Canceled
}

func (r *stallReader) Close() error { r.closed = true; return nil }

func TestWatchedBody_StallSurfacesTheIdleCause(t *testing.T) {
	ctx, dog := newAttemptWatchdog(context.Background(), 40*time.Millisecond)
	inner := &stallReader{ctx: ctx, data: strings.NewReader("partial")}
	body := dog.watch(inner)
	got, err := io.ReadAll(body)
	if string(got) != "partial" {
		t.Errorf("read %q before the stall, want %q", got, "partial")
	}
	if !errors.Is(err, errUpstreamIdle) {
		t.Errorf("stalled read error = %v, want errUpstreamIdle", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !inner.closed {
		t.Error("Close did not close the upstream body")
	}
}

func TestWatchedBody_ReadsRestartTheBound(t *testing.T) {
	ctx, dog := newAttemptWatchdog(context.Background(), 60*time.Millisecond)
	pr, pw := io.Pipe()
	body := dog.watch(pr)
	go func() {
		for i := 0; i < 8; i++ { // 160ms of steady bytes against a 60ms bound
			time.Sleep(20 * time.Millisecond)
			_, _ = pw.Write([]byte("x"))
		}
		_ = pw.Close()
	}()
	got, err := io.ReadAll(body)
	if err != nil || len(got) != 8 {
		t.Fatalf("read %q err %v, want 8 bytes and no error", got, err)
	}
	if ctx.Err() != nil {
		t.Errorf("steady reads still tripped the watchdog: %v", context.Cause(ctx))
	}
	_ = body.Close()
	if ctx.Err() == nil {
		t.Error("Close did not release the attempt context")
	}
}
