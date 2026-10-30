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

// The Vertex path carries the qualified model as its publisher and model
// segments; only generateContent and streamGenerateContent under v1 and
// v1beta1 resolve.
func TestVertexPathsServed(t *testing.T) {
	served := []string{
		"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent",
		"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:streamGenerateContent",
		"/v1beta1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent",
		"/v1beta1/projects/p/locations/global/publishers/prov/models/gemini-2.5-flash:streamGenerateContent",
	}
	for _, path := range served {
		a, ok := adapterForPath(path)
		if !ok || a.formatName() != providerTypeVertex {
			t.Errorf("%s: adapter %v, %v; want the Vertex adapter", path, a, ok)
			continue
		}
		if got := a.modelFromPath(path); got != "prov/gemini-2.5-flash" {
			t.Errorf("%s: modelFromPath = %q, want prov/gemini-2.5-flash", path, got)
		}
	}
	notServed := []string{
		// %2F decoded by the mux: one segment too many.
		"/v1/projects/p/locations/l/publishers/google/models/prov/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:countTokens",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:rawPredict",
		"/v1/projects/p/publishers/prov/models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers//models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini",
		"/v2/projects/p/locations/l/publishers/prov/models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:generateContent/extra",
		"/v1/projects/nonsense",
		"/v1/nonsense",
	}
	for _, path := range notServed {
		if a, ok := adapterForPath(path); ok {
			t.Errorf("%s resolved to %v, want no adapter", path, a)
		}
	}
	for _, a := range []providerAdapter{anthropicAdapter{}, openaiAdapter{}} {
		if got := a.modelFromPath(served[0]); got != "" {
			t.Errorf("%s modelFromPath = %q, want empty: the model travels in the body", a.formatName(), got)
		}
	}
}

// upstreamPath points the publisher at google, writes the raw model id, and
// sets alt=sse on streamGenerateContent only.
func TestVertexUpstreamPath(t *testing.T) {
	v := vertexAdapter{}
	cases := []struct{ in, model, want string }{
		{"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent", "gemini-2.5-flash",
			"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"},
		// A fallback edge's mapped model replaces the inbound one.
		{"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent", "gemini-2.0-flash",
			"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.0-flash:generateContent"},
		{"/v1beta1/projects/p/locations/global/publishers/prov/models/gemini-2.5-flash:streamGenerateContent", "gemini-2.5-flash",
			"/v1beta1/projects/p/locations/global/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse"},
		// Project and location survive, escaped for the URL.
		{"/v1/projects/my proj/locations/l?x/publishers/prov/models/m:generateContent", "m",
			"/v1/projects/my%20proj/locations/l%3Fx/publishers/google/models/m:generateContent"},
		// A path that is not a Vertex path passes through.
		{"/v1/chat/completions", "m", "/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := v.upstreamPath(c.in, c.model); got != c.want {
			t.Errorf("upstreamPath(%q, %q) = %q, want %q", c.in, c.model, got, c.want)
		}
	}
}

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

	t.Run("invalid key", func(t *testing.T) {
		_, err := newCache().token(ctx, nil, "prov", "not-a-key", time.Second)
		if !errors.Is(err, gcpauth.ErrInvalidKey) {
			t.Errorf("err = %v, want ErrInvalidKey", err)
		}
	})
}

func TestVertexUsageExtraction(t *testing.T) {
	v := vertexAdapter{}
	u, ok := v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12}}`))
	if !ok || u.InputTokens != 88 || u.OutputTokens != 12 {
		t.Errorf("buffered usage wrong: %+v ok=%v", u, ok)
	}
	var acc Usage
	v.accumulateStreamUsage([]byte(`{"candidates":[{"content":{}}]}`), &acc)
	v.accumulateStreamUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`), &acc)
	if acc.InputTokens != 5 || acc.OutputTokens != 3 {
		t.Errorf("stream usage wrong: %+v", acc)
	}
}

// Grounding with Google Search reports executed queries in
// groundingMetadata.webSearchQueries; the adapter counts them as the
// "google_search" server tool.
func TestVertexUsageExtraction_Grounding(t *testing.T) {
	v := vertexAdapter{}
	u, ok := v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12},` +
		`"candidates":[{"groundingMetadata":{"webSearchQueries":["euro 2024 winner","who won euro 2024"]}}]}`))
	if !ok || u.ServerTools["google_search"] != 2 {
		t.Errorf("grounded usage = %+v ok=%v, want google_search:2", u, ok)
	}

	// Streaming: groundingMetadata arrives on its own chunk before the final
	// usageMetadata chunk; both must survive.
	var acc Usage
	v.accumulateStreamUsage([]byte(`{"candidates":[{"groundingMetadata":{"webSearchQueries":["q1"]}}]}`), &acc)
	v.accumulateStreamUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`), &acc)
	if acc.InputTokens != 5 || acc.OutputTokens != 3 || acc.ServerTools["google_search"] != 1 {
		t.Errorf("grounded stream usage = %+v", acc)
	}

	// Ungrounded responses report no server tools.
	u, _ = v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1},"candidates":[{}]}`))
	if u.ServerTools != nil {
		t.Errorf("ungrounded response must yield nil ServerTools: %v", u.ServerTools)
	}
}

func TestVertexCredentialHeader(t *testing.T) {
	h := http.Header{}
	vertexAdapter{}.injectCredential(h, "ya29.minted-token")
	if h.Get("Authorization") != "Bearer ya29.minted-token" {
		t.Errorf("vertex credential must be a bearer token, got %q", h.Get("Authorization"))
	}
}
