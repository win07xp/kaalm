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
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A request whose caller left before the relay started counts as
// client_closed: a 2xx stream, which arrives unread and can no longer be
// read, counts as usage missing, and every outcome settles zero. A
// non-streamed 2xx never reaches settleCallerLeft: its relay reads it.
func TestSettleCallerLeft(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		noResponse  bool
		stream      bool
		wantInput   int64
		wantMissing float64
		// zeroSettle: the admission slot is freed with a zero cost.
		zeroSettle bool
	}{
		{name: "2xx stream", status: 200, stream: true,
			body:        "data: {\"id\":\"x\",\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n",
			wantMissing: 1, zeroSettle: true},
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
				res.resp = &http.Response{StatusCode: tc.status, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(tc.body))}
				if tc.stream {
					res.resp.Header.Set("Content-Type", "text/event-stream")
				}
				if tc.status >= 300 {
					res.body = []byte(tc.body)
				}
			}
			gone, cancel := context.WithCancel(context.Background())
			cancel()
			model, answered := "m1", "prov"
			h.server.settleCallerLeft(gone, res, "team-a", &model, &answered)

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errBody fails every read and records that the body was closed.
type errBody struct{ closed bool }

func (*errBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func (b *errBody) Close() error { b.closed = true; return nil }

// forwardOnceHarness points the upstream client at answer and returns what
// forwardOnce needs to call it.
func forwardOnceHarness(t *testing.T, answer func(*http.Request) *http.Response) func(context.Context) forwardResult {
	t.Helper()
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	provider := h.store.providers["prov"]
	adapter, _ := adapterForPath("/v1/chat/completions")
	typeAdapter, _ := adapterForProviderType(provider.Spec.Type)
	h.server.upstream().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return answer(r), nil })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return func(ctx context.Context) forwardResult {
		return h.server.forwardOnce(ctx, r, provider, []byte(`{"model":"m1"}`), "/v1/chat/completions", adapter, typeAdapter, "m1")
	}
}

// A 2xx answer comes back unread, still watched: the walk never abandons a
// 2xx, and the relay reads it.
func TestForwardOnce_SuccessIsReturnedUnread(t *testing.T) {
	body := &errBody{}
	forward := forwardOnceHarness(t, func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: body, Request: r}
	})
	res := forward(context.Background())
	if res.resp == nil || res.resp.StatusCode != http.StatusOK || res.err != nil || res.body != nil || res.class != classNone {
		t.Fatalf("forwardOnce = %+v, want the unread 200", res)
	}
	if _, ok := res.resp.Body.(*watchedBody); !ok {
		t.Errorf("body %T, want the watched body", res.resp.Body)
	}
	if body.closed {
		t.Error("the 2xx body was closed before the relay")
	}
	_ = res.resp.Body.Close()
}

// A non-2xx body is read up to errorBodyLimit and the rest dropped. A failed
// read is a connect-class failure the walk falls back from.
func TestForwardOnce_ErrorBodyIsReadUpToTheLimit(t *testing.T) {
	big := strings.Repeat("e", 1<<20)
	failRead := false
	forward := forwardOnceHarness(t, func(r *http.Request) *http.Response {
		if failRead {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{},
				Body: io.NopCloser(iotest.ErrReader(errors.New("connection reset"))), Request: r}
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(big)), Request: r}
	})
	res := forward(context.Background())
	if len(res.body) != errorBodyLimit || res.class != classUpstream || !res.fallilable {
		t.Fatalf("forwardOnce = body %d bytes, class %v, fallbackable %v; want %d, upstream, true",
			len(res.body), res.class, res.fallilable, errorBodyLimit)
	}

	failRead = true
	res = forward(context.Background())
	if !res.fallilable || res.class != classConnect || res.err == nil {
		t.Errorf("read failure = %+v, want a fallilable connect-class error", res)
	}
}

// A non-streamed 2xx is read to its end whatever the caller does, so its
// attempt no longer follows the caller's cancel; a stream's still does.
func TestForwardOnce_NonStreamedSuccessOutlivesTheCaller(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		outlives    bool
	}{
		{contentType: "application/json", outlives: true},
		{contentType: "text/event-stream", outlives: false},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			forward := forwardOnceHarness(t, func(r *http.Request) *http.Response {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}},
					Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}
			})
			parent, cancel := context.WithCancel(context.Background())
			res := forward(parent)
			body, ok := res.resp.Body.(*watchedBody)
			if !ok {
				t.Fatalf("body %T, want the watched body", res.resp.Body)
			}
			defer func() { _ = body.Close() }()
			cancel()
			if tc.outlives {
				time.Sleep(50 * time.Millisecond)
				if err := body.dog.ctx.Err(); err != nil {
					t.Fatalf("the non-streamed answer's read ended with the caller: %v", context.Cause(body.dog.ctx))
				}
				return
			}
			select {
			case <-body.dog.ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("the stream's read outlived the caller")
			}
			if cause := context.Cause(body.dog.ctx); !errors.Is(cause, context.Canceled) {
				t.Errorf("cause = %v, want context.Canceled", cause)
			}
		})
	}
}
