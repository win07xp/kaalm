package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// formatMismatchVertexPath names provider "prov" and model "m1" in the path.
const formatMismatchVertexPath = "/v1/projects/p/locations/global/publishers/prov/models/m1:generateContent"

// TestProxy_FormatMismatchIsRejected: a request whose path format is not the
// primary provider type's format is rejected with 400 invalid_request before
// it reaches the provider, and the message names the type and the path to
// call. openai and openai-compatible share one format.
func TestProxy_FormatMismatchIsRejected(t *testing.T) {
	cases := []struct {
		name, providerType, path string
		wantPath                 string // empty: the request is accepted
	}{
		{"messages to openai", providerTypeOpenAI, "/v1/messages", "/v1/chat/completions"},
		{"messages to openai-compatible", providerTypeOpenAICompatible, "/v1/messages", "/v1/chat/completions"},
		{"chat completions to anthropic", providerTypeAnthropic, "/v1/chat/completions", "/v1/messages"},
		{"legacy completions to anthropic", providerTypeAnthropic, "/v1/completions", "/v1/messages"},
		{"messages to google-vertex", providerTypeVertex, "/v1/messages", ":generateContent"},
		{"chat completions to google-vertex", providerTypeVertex, "/v1/chat/completions", ":generateContent"},
		{"vertex path to openai", providerTypeOpenAI, formatMismatchVertexPath, "/v1/chat/completions"},
		{"vertex path to anthropic", providerTypeAnthropic, formatMismatchVertexPath, "/v1/messages"},

		{"chat completions to openai", providerTypeOpenAI, "/v1/chat/completions", ""},
		{"legacy completions to openai", providerTypeOpenAI, "/v1/completions", ""},
		{"chat completions to openai-compatible", providerTypeOpenAICompatible, "/v1/chat/completions", ""},
		{"legacy completions to openai-compatible", providerTypeOpenAICompatible, "/v1/completions", ""},
		{"messages to anthropic", providerTypeAnthropic, "/v1/messages", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reject := c.wantPath != ""
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				if reject {
					t.Error("a mismatched request reached the provider")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp-1"}`))
			})
			h.seedRoute()
			h.store.providers["prov"].Spec.Type = c.providerType
			cert := agentCert(t, h.ca)
			body := map[string]any{"model": "prov/m1", "max_tokens": 5, "messages": []any{}}
			if strings.HasPrefix(c.path, "/v1/projects/") {
				body = map[string]any{"contents": []any{}}
			}
			resp := postJSON(t, h.client(&cert), h.url(c.path), body, nil)
			defer func() { _ = resp.Body.Close() }()
			if !reject {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s to %s = %d, want 200", c.path, c.providerType, resp.StatusCode)
				}
				return
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s to %s = %d, want 400", c.path, c.providerType, resp.StatusCode)
			}
			var envelope struct {
				Error errorBody `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatalf("decoding error envelope: %v", err)
			}
			if envelope.Error.Type != errInvalidRequest {
				t.Errorf("error type %q, want %q", envelope.Error.Type, errInvalidRequest)
			}
			if envelope.Error.Provider != "prov" {
				t.Errorf("error provider %q, want prov", envelope.Error.Provider)
			}
			msg := envelope.Error.Message
			if !strings.Contains(msg, `"`+c.providerType+`"`) || !strings.Contains(msg, c.wantPath) {
				t.Errorf("message %q must name type %q and path %q", msg, c.providerType, c.wantPath)
			}
		})
	}
}

// TestProxy_FormatMismatchConsumesNothing: a rejected mismatch takes no
// request or token reservation and records no spend, so the next matching
// request is admitted under a one-request-per-minute limit.
func TestProxy_FormatMismatchConsumesNothing(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"m1",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`))
	})
	h.store.providers["prov"].Spec.RateLimits = kaalmv1beta1.ModelProviderRateLimits{
		TokensPerMinute: 1000, RequestsPerMinute: 1,
	}
	cert := agentCert(t, h.ca)
	client := h.client(&cert)

	resp := postJSON(t, client, h.url("/v1/messages"),
		map[string]any{"model": "prov/m1", "max_tokens": 10, "messages": []any{
			map[string]any{"role": "user", "content": "hi"}}}, nil)
	if got := resp.StatusCode; got != http.StatusBadRequest {
		_ = resp.Body.Close()
		t.Fatalf("mismatched request = %d, want 400", got)
	}
	if got := errType(t, resp); got != errInvalidRequest {
		t.Errorf("error type %q, want %q", got, errInvalidRequest)
	}
	if u := h.spend.Total("team-a", "prov", "m1"); !u.isZero() {
		t.Errorf("spend after a rejected request = %+v, want zero", u)
	}

	next := postJSON(t, client, h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "max_tokens": 10, "messages": []any{
			map[string]any{"role": "user", "content": "hi"}}}, nil)
	_ = next.Body.Close()
	if next.StatusCode != http.StatusOK {
		t.Errorf("matching request after the rejection = %d, want 200", next.StatusCode)
	}
}
