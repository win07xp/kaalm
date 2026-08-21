// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const testParent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// Contract item 8 end to end: the trace context on the delivery request
// reaches the handler's ctx and rides every gateway call made with it.
func TestServe_TraceContextReachesHandlerAndGatewayCalls(t *testing.T) {
	pki := newTestPKI(t)

	gwSeen := make(chan [2]string, 2)
	gwSrv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gwSeen <- [2]string{r.Header.Get("Traceparent"), r.Header.Get("Tracestate")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	g := newGateway(gwSrv.URL, testReloader(t, pki))

	h := func(ctx context.Context, _ Envelope) (Response, error) {
		parent, state := TraceContext(ctx)
		resp, err := g.Post(ctx, "/v1/chat/completions", map[string]any{"model": "p/m"})
		if err != nil {
			return Response{}, err
		}
		_ = resp.Body.Close()
		return Response{Content: parent + "|" + state}, nil
	}
	_, addr, _ := startAgent(t, pki, agentSANs, t.TempDir(), "", h)
	client := pki.clientFor(t, "gateway", gatewaySANLocal)

	post := func(messageID, parent, state string) Response {
		t.Helper()
		raw, _ := json.Marshal(Envelope{MessageID: messageID, Content: "x"})
		req, err := http.NewRequest(http.MethodPost, addr+"/v1/message", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if parent != "" {
			req.Header.Set("traceparent", parent)
		}
		if state != "" {
			req.Header.Set("tracestate", state)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := post("t1", testParent, "kaalm=1"); got.Content != testParent+"|kaalm=1" {
		t.Errorf("handler saw %q, want the delivered trace context", got.Content)
	}
	if seen := <-gwSeen; seen != [2]string{testParent, "kaalm=1"} {
		t.Errorf("gateway call carried %v, want the delivered trace context", seen)
	}

	// A delivery without context: nothing is invented, nothing leaks over.
	if got := post("t2", "", ""); got.Content != "|" {
		t.Errorf("handler saw %q for an untraced delivery, want empty", got.Content)
	}
	if seen := <-gwSeen; seen != [2]string{"", ""} {
		t.Errorf("gateway call carried %v for an untraced delivery, want none", seen)
	}
}
