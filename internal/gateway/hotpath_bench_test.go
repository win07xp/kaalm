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

// Benchmarks for the pure functions on the gateway's request paths, the
// ones CPU and allocation profiles under the load baseline ranked (#174).
// They need no cluster and are the regression guard the load harness
// cannot be: run `make bench` before and after a change to one of these
// paths and compare with benchstat.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// openAIResponse is the shape a provider answers a chat completion with,
// sized like a short reply.
var openAIResponse = []byte(`{"id":"chatcmpl-123","object":"chat.completion","created":1757600000,` +
	`"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok from mock"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)

func BenchmarkExtractUsageOpenAI(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := (openaiAdapter{}).extractUsage(openAIResponse); !ok {
			b.Fatal("usage not found")
		}
	}
}

func BenchmarkCopyForwardedHeaders(b *testing.B) {
	src := http.Header{
		"Authorization":     {"Bearer token"},
		"Content-Type":      {"application/json"},
		"Accept":            {"application/json"},
		"User-Agent":        {"openai-python/1.0"},
		"Traceparent":       {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		"X-Request-Id":      {"req-1"},
		"Accept-Encoding":   {"gzip, deflate"},
		"Connection":        {"keep-alive"},
		"Content-Length":    {"512"},
		"X-Forwarded-Proto": {"https"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst := make(http.Header, len(src))
		copyForwardedHeaders(dst, src)
	}
}

func BenchmarkParseWorkloadSAN(b *testing.B) {
	cert := certWithSANs("sup.team-a", "sup.team-a.svc", "sup.team-a.svc.cluster.local")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseWorkloadSAN(cert); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBudgetLedgerAddAndEnforce(b *testing.B) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	ledger := NewBudgetLedger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ledger.Add(p, "team-a", "agent/sup", 0.000001)
		if d := ledger.Enforce(p, "team-a"); d.Action == kaalmv1beta1.BudgetActionBlock {
			b.Fatal("ceiling reached inside the benchmark")
		}
	}
}

func BenchmarkVerifyHMACHeader(b *testing.B) {
	secret := []byte("shh")
	body := []byte(`{"text":"hello","user":"u1"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	header := hex.EncodeToString(expected)
	cfg := &kaalmv1beta1.ChannelHMAC{Header: "X-Sig", Algorithm: "sha256"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !verifyHMACHeader(header, cfg, expected) {
			b.Fatal("signature rejected")
		}
	}
}

func BenchmarkSignCallback(b *testing.B) {
	auth := &kaalmv1beta1.ChannelAuth{Type: authTypeHMAC,
		HMAC: &kaalmv1beta1.ChannelHMAC{Header: "X-Sig", Algorithm: "sha256"}}
	body := []byte(`{"requestId":"req-1","content":"ok"}`)
	now := time.Unix(1757600000, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "https://receiver.example/cb", nil)
		signCallback(req, auth, "shh", "req-1", body, now)
	}
}

func BenchmarkDeliveryOutcome(b *testing.B) {
	errs := []error{
		fmt.Errorf("Post: %w", &dialStageError{stage: dialStageConnect, err: errors.New("i/o timeout")}),
		errors.New("agent returned 503"),
		errors.New("agent returned 200 with a malformed response envelope"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		deliveryOutcome(errs[i%len(errs)])
	}
}

// BenchmarkLLMProxyMTLS runs the whole LLM proxy path in process: an mTLS
// caller, route authorization, the forwarded-header contract, the upstream
// round trip to a local provider, usage extraction, and spend accounting.
// TLS on both hops is included, so the number is an upper bound on the
// per-request cost the gateway phase measures without the cluster.
func BenchmarkLLMProxyMTLS(b *testing.B) {
	h := newHarness(b, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	})
	h.seedRoute()
	cert := agentCert(b, h.ca)
	client := h.client(&cert)
	body := map[string]any{"model": "prov/m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := postJSON(b, client, h.url("/v1/chat/completions"), body, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
	}
}
