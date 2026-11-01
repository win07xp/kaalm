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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/win07xp/kaalm/internal/gcpauth"
)

// tokenEndpoint is a fake token endpoint that counts mints and answers
// with expiresIn (omitted when zero). A blocking endpoint holds each mint
// until release closes.
type tokenEndpoint struct {
	srv     *httptest.Server
	mints   atomic.Int64
	status  atomic.Int64
	release chan struct{} // when set, each mint waits for it to close
}

func newTokenEndpoint(t *testing.T, expiresIn int, blocking bool) *tokenEndpoint {
	t.Helper()
	e := &tokenEndpoint{}
	if blocking {
		e.release = make(chan struct{})
	}
	e.status.Store(http.StatusOK)
	e.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := e.mints.Add(1)
		if e.release != nil {
			<-e.release
		}
		w.Header().Set("Content-Type", "application/json")
		if status := int(e.status.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		body := fmt.Sprintf(`{"access_token":"tok-%d","token_type":"Bearer"`, n)
		if expiresIn > 0 {
			body += fmt.Sprintf(`,"expires_in":%d`, expiresIn)
		}
		_, _ = w.Write([]byte(body + "}"))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// The token cache reuses a token until five minutes before it expires,
// re-mints on a rotated key, treats a token without expires_in as an hour
// long, and coalesces a burst into one mint.
func TestVertexTokenCache(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	newCache := func() *vertexTokenCache {
		return &vertexTokenCache{now: func() time.Time { return clock }}
	}

	t.Run("expiry and rotation", func(t *testing.T) {
		e := newTokenEndpoint(t, 3600, false)
		c := newCache()
		key := vertexServiceAccountJSON(t, e.srv.URL+"/token")
		get := func(credential string) string {
			t.Helper()
			tok, err := c.token(ctx, e.srv.Client(), "prov", credential, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}
		start := clock
		first := get(key)
		if got := get(key); got != first || e.mints.Load() != 1 {
			t.Errorf("second read = %q after %d mints, want the cached %q", got, e.mints.Load(), first)
		}
		// Within the refresh margin of an hour-long token: still cached at
		// 54 minutes, re-minted at 56.
		clock = start.Add(54 * time.Minute)
		_ = get(key)
		if e.mints.Load() != 1 {
			t.Errorf("mints at 54m = %d, want 1", e.mints.Load())
		}
		clock = start.Add(56 * time.Minute)
		if got := get(key); got == first || e.mints.Load() != 2 {
			t.Errorf("at 56m got %q after %d mints, want a fresh token", got, e.mints.Load())
		}
		// A rotated Secret (a different key value) re-mints at once.
		rotated := vertexServiceAccountJSON(t, e.srv.URL+"/token?rotated")
		_ = get(rotated)
		if e.mints.Load() != 3 {
			t.Errorf("mints after rotation = %d, want 3", e.mints.Load())
		}
		clock = start
	})

	t.Run("no expires_in", func(t *testing.T) {
		e := newTokenEndpoint(t, 0, false)
		c := newCache()
		key := vertexServiceAccountJSON(t, e.srv.URL+"/token")
		start := clock
		for _, at := range []time.Duration{0, 54 * time.Minute, 56 * time.Minute} {
			clock = start.Add(at)
			if _, err := c.token(ctx, e.srv.Client(), "prov", key, 5*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		if e.mints.Load() != 2 {
			t.Errorf("mints = %d, want 2: an hour-long token refreshed once", e.mints.Load())
		}
		clock = start
	})

	t.Run("burst shares one mint", func(t *testing.T) {
		e := newTokenEndpoint(t, 3600, true)
		c := newCache()
		key := vertexServiceAccountJSON(t, e.srv.URL+"/token")
		var wg sync.WaitGroup
		tokens := make([]string, 20)
		errs := make([]error, 20)
		for i := range tokens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tokens[i], errs[i] = c.token(ctx, e.srv.Client(), "prov", key, 5*time.Second)
			}()
		}
		for e.mints.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // let the burst join the flight
		close(e.release)
		wg.Wait()
		for i := range tokens {
			if errs[i] != nil || tokens[i] != tokens[0] {
				t.Errorf("caller %d got %q, %v; want %q", i, tokens[i], errs[i], tokens[0])
			}
		}
		if e.mints.Load() != 1 {
			t.Errorf("mints = %d, want 1", e.mints.Load())
		}
	})

	t.Run("burst shares a refusal", func(t *testing.T) {
		e := newTokenEndpoint(t, 3600, true)
		e.status.Store(http.StatusUnauthorized)
		c := newCache()
		key := vertexServiceAccountJSON(t, e.srv.URL+"/token")
		var wg sync.WaitGroup
		errs := make([]error, 20)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = c.token(ctx, e.srv.Client(), "prov", key, 5*time.Second)
			}()
		}
		for e.mints.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		close(e.release)
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, gcpauth.ErrRejected) {
				t.Errorf("caller %d: err = %v, want ErrRejected", i, err)
			}
		}
		if e.mints.Load() != 1 {
			t.Errorf("mints = %d, want 1", e.mints.Load())
		}
	})

	t.Run("silent endpoint ends at the bound", func(t *testing.T) {
		e := newTokenEndpoint(t, 3600, true)
		t.Cleanup(func() { close(e.release) }) // runs before the server closes
		c := newCache()
		key := vertexServiceAccountJSON(t, e.srv.URL+"/token")
		for i := 1; i <= 2; i++ {
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := c.token(ctx, e.srv.Client(), "prov", key, 200*time.Millisecond)
				done <- err
			}()
			select {
			case err := <-done:
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Errorf("call %d returned after %v, want about 200ms", i, elapsed)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("call %d: err = %v, want context.DeadlineExceeded", i, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("call %d: the mint ignored its bound", i)
			}
			// The timed-out flight is over, so each call starts its own mint.
			if got := e.mints.Load(); got != int64(i) {
				t.Errorf("mints after call %d = %d, want %d", i, got, i)
			}
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		_, err := newCache().token(ctx, nil, "prov", "not-a-key", time.Second)
		if !errors.Is(err, gcpauth.ErrInvalidKey) {
			t.Errorf("err = %v, want ErrInvalidKey", err)
		}
	})
}
