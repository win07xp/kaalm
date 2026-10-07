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
	"net/http/httptest"
	"strings"
	"testing"
)

// When the caller leaves, the upstream read can end in a clean io.EOF
// instead of the context error. The relay must still count the caller
// leaving, not a stream relayed in full.
func TestRelayStream_CleanEndAfterCallerLeftIsClientClosed(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	adapter, ok := adapterForProviderType("openai")
	if !ok {
		t.Fatal("no openai adapter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")),
	}
	outcome := h.server.relayStream(ctx, httptest.NewRecorder(), resp, adapter, nil, formatForType("openai"),
		"team-a", "sup", h.store.providers["prov"], "m1", nil, nil)
	if outcome != outcomeClientClosed {
		t.Errorf("outcome = %q, want %q", outcome, outcomeClientClosed)
	}
}
