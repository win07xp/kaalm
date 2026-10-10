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
	"io"
	"net/http"
	"strings"
	"testing"

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
