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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// vertexTestKeyPEM is one RSA key for the package's tests: generating a
// 2048-bit key per test would dominate their run time.
var vertexTestKeyPEM = sync.OnceValues(func() (string, error) {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
})

// vertexServiceAccountJSON builds a service-account JSON key whose
// token_uri is tokenURL.
func vertexServiceAccountJSON(t testing.TB, tokenURL string) string {
	t.Helper()
	pemKey, err := vertexTestKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "p", "private_key_id": "kid",
		"private_key": pemKey, "client_email": "sa@p.iam.gserviceaccount.com", "token_uri": tokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const (
	vertexGeneratePath = "/v1/projects/p/locations/l/publishers/prov/models/gemini:generateContent"
	vertexStreamPath   = "/v1/projects/p/locations/l/publishers/prov/models/gemini:streamGenerateContent"
	vertexResponse     = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33}}`
)

// vertexUpstream answers the token endpoint on /token (a token),
// /token-reject (a refusal of the key), and /token-silent (no answer for 3s,
// or until the caller hangs up), and hands every other path to gen.
func vertexUpstream(gen http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ya29.test","token_type":"Bearer","expires_in":3600}`))
		case "/token-reject":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		case "/token-silent":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		default:
			gen(w, r)
		}
	}
}

// vertexJSON answers every Vertex call with vertexResponse.
func vertexJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(vertexResponse))
}

// vertexHarness turns the harness's "prov" into a google-vertex provider
// serving model "gemini", whose key mints at the harness upstream's /token.
func vertexHarness(t *testing.T, gen http.HandlerFunc) (*harness, *tls.Certificate) {
	t.Helper()
	h := newHarness(t, vertexUpstream(gen))
	h.seedRoute()
	p := h.store.providers["prov"]
	p.Spec.Type = providerTypeVertex
	p.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "gemini"}}
	h.store.creds["prov"] = vertexServiceAccountJSON(t, h.upstream.URL+"/token")
	cert := agentCert(t, h.ca)
	return h, &cert
}

// addVertexBackup adds a google-vertex fallback "backup" on the harness
// upstream with its own accepted key, wired onto "prov".
func (h *harness) addVertexBackup(t *testing.T, models []kaalmv1beta1.ModelProviderModel, modelMap map[string]string) {
	t.Helper()
	h.store.providers["backup"] = &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1ObjectMeta("backup"),
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: providerTypeVertex, Endpoint: h.upstream.URL,
			AllowedNamespaces: []string{"team-*"}, Models: models,
		},
	}
	h.store.creds["backup"] = vertexServiceAccountJSON(t, h.upstream.URL+"/token")
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup", ModelMap: modelMap}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
}

func vertexRequestBody() map[string]any {
	return map[string]any{"contents": []any{map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
}

// capturedUpstream returns the upstream requests captured so far.
func capturedUpstream(h *harness) []*capturedRequest {
	var out []*capturedRequest
	for {
		select {
		case r := <-h.upreqs:
			out = append(out, r)
		default:
			return out
		}
	}
}

// vertexCalls splits captured upstream requests into token mints and the
// rest.
func vertexCalls(reqs []*capturedRequest) (mints, calls []*capturedRequest) {
	for _, r := range reqs {
		if strings.HasPrefix(r.path, "/token") {
			mints = append(mints, r)
		} else {
			calls = append(calls, r)
		}
	}
	return mints, calls
}

// waitSpend waits for a usage record to land (a stream settles after its
// last flush).
func waitSpend(h *harness, namespace, provider, model string) Usage {
	var u Usage
	for i := 0; i < 200; i++ {
		if u = h.spend.Total(namespace, provider, model); !u.isZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return u
}

// A google-vertex provider is served on the Vertex path: the gateway mints
// an access token from the service-account key, sends it as the bearer
// credential, rewrites the path for the upstream, and meters the answer.
func TestLLMProxy_VertexGenerateContent(t *testing.T) {
	h, cert := vertexHarness(t, vertexJSON)

	resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(),
		map[string]string{"X-Goog-Api-Key": "caller-key"})
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", resp.StatusCode, got)
	}
	if string(got) != vertexResponse {
		t.Errorf("body %s, want the upstream's verbatim", got)
	}

	mints, calls := vertexCalls(capturedUpstream(h))
	if len(mints) != 1 || len(calls) != 1 {
		t.Fatalf("upstream saw %d mints and %d calls, want 1 and 1", len(mints), len(calls))
	}
	form, err := url.ParseQuery(string(mints[0].raw))
	if err != nil || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || form.Get("assertion") == "" {
		t.Errorf("token request form = %v (%v), want a jwt-bearer grant with an assertion", form, err)
	}
	call := calls[0]
	if want := "/v1/projects/p/locations/l/publishers/google/models/gemini:generateContent"; call.path != want || call.query != "" {
		t.Errorf("upstream path %q?%q, want %q with no query", call.path, call.query, want)
	}
	if got := call.header.Get("Authorization"); got != "Bearer ya29.test" {
		t.Errorf("upstream Authorization %q, want the minted token", got)
	}
	if got := call.header.Get("X-Goog-Api-Key"); got != "" {
		t.Errorf("the caller's x-goog-api-key reached the upstream: %q", got)
	}
	if _, ok := call.body["contents"]; !ok || call.body["model"] != nil {
		t.Errorf("upstream body %s, want the caller's contents and no model member", call.raw)
	}
	if u := h.spend.Total("team-a", "prov", "gemini"); u.InputTokens != 11 || u.OutputTokens != 22 {
		t.Errorf("spend = %+v, want 11 in and 22 out", u)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmRequests.WithLabelValues("prov", "gemini", "team-a", outcomeOK)); got != 1 {
		t.Errorf("kaalm_llm_requests_total ok = %v, want 1", got)
	}

	// The second request reuses the cached token.
	resp = postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second call = %d, want 200", resp.StatusCode)
	}
	if mints, calls := vertexCalls(capturedUpstream(h)); len(mints) != 0 || len(calls) != 1 {
		t.Errorf("second call: %d mints and %d calls, want 0 and 1", len(mints), len(calls))
	}
}

// streamGenerateContent relays the SSE stream verbatim and settles the
// usage of its last chunk; the gateway adds ?alt=sse upstream.
func TestLLMProxy_VertexStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hel"}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP",` +
			`"groundingMetadata":{"webSearchQueries":["q1","q2"]}}],` +
			`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`,
		``,
		``,
	}, "\n")
	h, cert := vertexHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})

	resp := postJSON(t, h.client(cert), h.url(vertexStreamPath), vertexRequestBody(), nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), `"text":"hel"`) || !strings.Contains(string(got), `"usageMetadata"`) {
		t.Errorf("relayed stream %q is missing the upstream's events", got)
	}
	if u := waitSpend(h, "team-a", "prov", "gemini"); u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Errorf("spend = %+v, want 5 in and 3 out", u)
	}
	_, calls := vertexCalls(capturedUpstream(h))
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d calls, want 1", len(calls))
	}
	if want := "/v1/projects/p/locations/l/publishers/google/models/gemini:streamGenerateContent"; calls[0].path != want || calls[0].query != "alt=sse" {
		t.Errorf("upstream %q?%q, want %q?alt=sse", calls[0].path, calls[0].query, want)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmServerTools.WithLabelValues("prov", "team-a", "google_search")); got != 2 {
		t.Errorf("google_search server tool count = %v, want 2", got)
	}
}

// A token endpoint that refuses the key is a credential failure: the walk
// moves to a fallback and the provider gets a CredentialsInvalid event;
// with no fallback the caller gets 502.
func TestLLMProxy_VertexTokenRejected(t *testing.T) {
	t.Run("fallback serves", func(t *testing.T) {
		h, cert := vertexHarness(t, vertexJSON)
		capture := &eventCapture{}
		h.server.Recorder = capture
		h.store.creds["prov"] = vertexServiceAccountJSON(t, h.upstream.URL+"/token-reject")
		h.addVertexBackup(t, []kaalmv1beta1.ModelProviderModel{{ID: "gemini"}}, nil)

		resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200 from the backup", resp.StatusCode)
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		found := false
		for i, reason := range capture.reasons {
			if reason == kaalmv1beta1.ReasonCredentialsInvalid {
				p, _ := capture.objects[i].(*kaalmv1beta1.ModelProvider)
				found = p != nil && p.Name == "prov" && strings.Contains(capture.messages[i], "token endpoint")
			}
		}
		if !found {
			t.Errorf("no CredentialsInvalid event on prov naming the token endpoint: %v %v", capture.reasons, capture.messages)
		}
		if got := testutil.ToFloat64(h.server.Metrics.llmFallback.WithLabelValues("prov", "backup", fallbackReasonSuccess)); got != 1 {
			t.Errorf("fallback {prov,backup,success} = %v, want 1", got)
		}
		if u := h.spend.Total("team-a", "backup", "gemini"); u.InputTokens != 11 {
			t.Errorf("backup spend = %+v, want 11 in", u)
		}
	})
	t.Run("no fallback", func(t *testing.T) {
		h, cert := vertexHarness(t, vertexJSON)
		h.store.creds["prov"] = vertexServiceAccountJSON(t, h.upstream.URL+"/token-reject")
		resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status %d, want 502", resp.StatusCode)
		}
		if got := errType(t, resp); got != errProviderError {
			t.Errorf("error type %q, want %q", got, errProviderError)
		}
		if _, calls := vertexCalls(capturedUpstream(h)); len(calls) != 0 {
			t.Errorf("%d calls reached the Vertex upstream, want 0", len(calls))
		}
	})
}

// A Secret value that is not a service-account key is refused like a
// credential the store refuses: 503, nothing sent, one warning a minute.
func TestLLMProxy_VertexInvalidKey(t *testing.T) {
	buf := captureSlog(t)
	h, cert := vertexHarness(t, vertexJSON)
	h.store.creds["prov"] = "not-a-key"
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.credentialLog.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("call %d: status %d, want 503", i, resp.StatusCode)
		}
		if got := errType(t, resp); got != errProviderUnavailable {
			t.Errorf("call %d: error type %q, want %q", i, got, errProviderUnavailable)
		}
	}
	if n := len(capturedUpstream(h)); n != 0 {
		t.Errorf("%d requests reached the upstream, want 0", n)
	}
	recs := logRecords(t, buf, "llm credential unavailable")
	if len(recs) != 1 || recs[0]["provider"] != "prov" {
		t.Fatalf("want one credential warning naming prov, got %v", recs)
	}
	if e, _ := recs[0]["error"].(string); !strings.Contains(e, "invalid service-account key") {
		t.Errorf("warning error %q should say the key is invalid", e)
	}
}

// A token endpoint the gateway cannot reach is a connect failure, logged as
// llm access token unavailable at most once a minute.
func TestLLMProxy_VertexTokenEndpointUnreachable(t *testing.T) {
	buf := captureSlog(t)
	h, cert := vertexHarness(t, vertexJSON)
	h.store.creds["prov"] = vertexServiceAccountJSON(t, "https://127.0.0.1:1/token")
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.credentialLog.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("call %d: status %d, want 503", i, resp.StatusCode)
		}
		if got := errType(t, resp); got != errProviderUnavailable {
			t.Errorf("call %d: error type %q, want %q", i, got, errProviderUnavailable)
		}
	}
	if recs := logRecords(t, buf, "llm access token unavailable"); len(recs) != 1 || recs[0]["provider"] != "prov" {
		t.Errorf("want one access-token warning naming prov, got %v", recs)
	}
	if n := len(capturedUpstream(h)); n != 0 {
		t.Errorf("%d requests reached the upstream, want 0", n)
	}
}

// A token endpoint that accepts the request and stays silent is bounded by
// the upstream timeout: the caller gets 504 provider_timeout and the gateway
// logs llm access token unavailable once a minute.
func TestLLMProxy_VertexTokenEndpointSilent(t *testing.T) {
	buf := captureSlog(t)
	h, cert := vertexHarness(t, vertexJSON)
	h.server.Config.UpstreamTimeout = 200 * time.Millisecond
	h.store.creds["prov"] = vertexServiceAccountJSON(t, h.upstream.URL+"/token-silent")
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.credentialLog.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		start := time.Now()
		resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("call %d took %v, want about the 200ms upstream timeout", i, elapsed)
		}
		if resp.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("call %d: status %d, want 504", i, resp.StatusCode)
		}
		if got := errType(t, resp); got != errProviderTimeout {
			t.Errorf("call %d: error type %q, want %q", i, got, errProviderTimeout)
		}
	}
	if recs := logRecords(t, buf, "llm access token unavailable"); len(recs) != 1 || recs[0]["provider"] != "prov" {
		t.Errorf("want one access-token warning naming prov, got %v", recs)
	}
	_, calls := vertexCalls(capturedUpstream(h))
	if len(calls) != 0 {
		t.Errorf("%d Vertex calls reached the upstream, want 0", len(calls))
	}
}

// A budget degrade rewrites the model in the upstream path, and the spend
// lands on the degraded model.
func TestLLMProxy_VertexDegradeRewritesPath(t *testing.T) {
	h, cert := vertexHarness(t, vertexJSON)
	degradeTo := smallModel
	p := h.store.providers["prov"]
	p.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "degrade", DegradeTo: &degradeTo}},
	}
	p.Spec.Models = []kaalmv1beta1.ModelProviderModel{
		{ID: "gemini", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "30"},
		{ID: smallModel, CostPer1MInputTokens: "1", CostPer1MOutputTokens: "3"},
	}
	h.server.Budget.Add(p, "team-a", "agent/test", 85)

	resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	_, calls := vertexCalls(capturedUpstream(h))
	if len(calls) != 1 || calls[0].path != "/v1/projects/p/locations/l/publishers/google/models/"+smallModel+":generateContent" {
		t.Fatalf("upstream calls %v, want one on the degraded model", calls)
	}
	if u := h.spend.Total("team-a", "prov", smallModel); u.InputTokens != 11 {
		t.Errorf("spend on %s = %+v, want 11 in", smallModel, u)
	}
}

// A same-type fallback edge with a modelMap writes the mapped model into
// the backup's path.
func TestLLMProxy_VertexFallbackModelMap(t *testing.T) {
	h, cert := vertexHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models/gemini:generateContent") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		vertexJSON(w, r)
	})
	h.server.Recorder = &recordingRecorder{}
	h.addVertexBackup(t, []kaalmv1beta1.ModelProviderModel{{ID: "gemini-lite"}}, map[string]string{"gemini": "gemini-lite"})

	resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 from the backup", resp.StatusCode)
	}
	_, calls := vertexCalls(capturedUpstream(h))
	if len(calls) != 2 || calls[1].path != "/v1/projects/p/locations/l/publishers/google/models/gemini-lite:generateContent" {
		t.Fatalf("upstream calls %d, want the primary then the backup on gemini-lite", len(calls))
	}
	if u := h.spend.Total("team-a", "backup", "gemini-lite"); u.InputTokens != 11 {
		t.Errorf("backup spend = %+v, want 11 in", u)
	}
}

// tokensPerMinute debits the input and output tokens of usageMetadata, and
// requestsPerMinute counts Vertex calls.
func TestLLMProxy_VertexRateLimitTokens(t *testing.T) {
	for _, c := range []struct {
		name       string
		limits     kaalmv1beta1.ModelProviderRateLimits
		retryAfter string
	}{
		// 30 - 33 = -3 tokens at 30/min: 6 seconds.
		{"tokens", kaalmv1beta1.ModelProviderRateLimits{TokensPerMinute: 30}, "6"},
		{"requests", kaalmv1beta1.ModelProviderRateLimits{RequestsPerMinute: 1}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, cert := vertexHarness(t, vertexJSON)
			h.server.RateLimiter = NewRateLimiter(func() int { return 1 })
			now := time.Now()
			h.server.RateLimiter.now = func() time.Time { return now }
			h.store.providers["prov"].Spec.RateLimits = c.limits

			resp := postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("first call = %d, want 200", resp.StatusCode)
			}
			resp = postJSON(t, h.client(cert), h.url(vertexGeneratePath), vertexRequestBody(), nil)
			if resp.StatusCode != http.StatusTooManyRequests {
				_ = resp.Body.Close()
				t.Fatalf("second call = %d, want 429", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); c.retryAfter != "" && got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
			if got := errType(t, resp); got != errRateLimited {
				t.Errorf("error type %q, want %q", got, errRateLimited)
			}
		})
	}
}

// A path under the Vertex prefixes that is not a Vertex call gets the
// constant 400, which never echoes the path.
func TestLLMProxy_VertexUnrecognizedPath(t *testing.T) {
	h, cert := vertexHarness(t, vertexJSON)
	resp := postJSON(t, h.client(cert), h.url("/v1/projects/p/locations/l/publishers/prov/models/secret-model:countTokens"),
		vertexRequestBody(), nil)
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Error errorBody `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || envelope.Error.Type != errInvalidRequest ||
		envelope.Error.Message != "unrecognized path" {
		t.Errorf("got %d %+v, want 400 invalid_request \"unrecognized path\"", resp.StatusCode, envelope.Error)
	}
	if n := len(capturedUpstream(h)); n != 0 {
		t.Errorf("%d requests reached the upstream, want 0", n)
	}
}
