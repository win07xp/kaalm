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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A request whose caller left settles what the gateway can read and counts
// as client_closed: a 2xx answer read in full settles its usage, a cut-off
// one counts as usage missing, and anything else settles zero.
func TestSettleCallerLeft(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		noResponse  bool
		wantInput   int64
		wantMissing float64
		// zeroSettle: the admission slot is freed with a zero cost.
		zeroSettle bool
	}{
		{name: "full answer", status: 200, body: `{"id":"x","usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			wantInput: 7},
		{name: "cut-off answer", status: 200, body: `{"id":"x","choices":[`, wantMissing: 1, zeroSettle: true},
		{name: "provider error", status: 400, body: `{"error":{}}`, zeroSettle: true},
		{name: "no response", noResponse: true, zeroSettle: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
			h.seedRoute()
			prov := h.store.providers["prov"]
			settled := -1.0
			res := forwardResult{provider: "prov", model: "m1", chosen: prov,
				settle: func(cost float64) { settled = cost }}
			if !tc.noResponse {
				res.resp = &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(""))}
				res.body = []byte(tc.body)
			}
			gone, cancel := context.WithCancel(context.Background())
			cancel()
			model, answered := "m1", "prov"
			h.server.settleCallerLeft(gone, res, openaiAdapter{}, "team-a", "agent/sup", &model, &answered, nil)

			if got := testutil.ToFloat64(h.server.Metrics.llmRequests.WithLabelValues("prov", "m1", "team-a", outcomeClientClosed)); got != 1 {
				t.Errorf("client_closed requests = %v, want 1", got)
			}
			if u := h.spend.Total("team-a", "prov", "m1"); u.InputTokens != tc.wantInput {
				t.Errorf("settled input tokens = %d, want %d", u.InputTokens, tc.wantInput)
			}
			if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != tc.wantMissing {
				t.Errorf("usage missing = %v, want %v", got, tc.wantMissing)
			}
			if tc.zeroSettle && settled != 0 {
				t.Errorf("settle = %v, want 0", settled)
			}
		})
	}
}

// cancelingBody returns data on the first read, then cancels the caller and
// fails the next read, as a transport does when the caller leaves mid-read.
type cancelingBody struct {
	data   string
	read   bool
	cancel context.CancelFunc
}

func (b *cancelingBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.data), nil
	}
	b.cancel()
	return 0, context.Canceled
}

func (*cancelingBody) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A caller that leaves while a 2xx answer is read hands the answer read so
// far to the handler, so a cut-off answer settles the same whether the read
// failed or ended cleanly.
func TestForwardOnce_CallerLeftMidReadKeepsTheAnswer(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	provider := h.store.providers["prov"]
	adapter, _ := adapterForPath("/v1/chat/completions")
	typeAdapter, _ := adapterForProviderType(provider.Spec.Type)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const partial = `{"id":"x","choices":[`
	h.server.upstream().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: &cancelingBody{data: partial, cancel: cancel}, Request: r}, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	res := h.server.forwardOnce(ctx, r, provider, []byte(`{"model":"m1"}`), "/v1/chat/completions", adapter, typeAdapter, "m1")
	if res.resp == nil || res.resp.StatusCode != http.StatusOK || res.err != nil || string(res.body) != partial {
		t.Fatalf("forwardOnce = %+v, want the 200 with the partial body and no error", res)
	}

	// Without the caller leaving, a failed read is still a connect-class
	// failure the walk falls back from.
	h.server.upstream().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(iotest.ErrReader(errors.New("connection reset"))), Request: r}, nil
	})
	res = h.server.forwardOnce(context.Background(), r, provider, []byte(`{"model":"m1"}`), "/v1/chat/completions", adapter, typeAdapter, "m1")
	if !res.fallilable || res.class != classConnect || res.err == nil {
		t.Errorf("read failure = %+v, want a fallilable connect-class error", res)
	}
}
