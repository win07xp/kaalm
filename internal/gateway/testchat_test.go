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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// postTestChat calls the handler directly (the ConsolePaths SAN gate is
// covered by TestAuthMatrix) and returns the recorder.
func postTestChat(h *userHarness, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/test-chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.server.handleTestChat(rec, req)
	return rec
}

func TestTestChat_RoundTrip(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"alive and well"}`))
	})
	h.seedChannel("sync")
	reg := prometheus.NewRegistry()
	h.server.Metrics = NewMetrics(reg)

	rec := postTestChat(h, `{"namespace":"team-a","agent":"sup","userId":"priya@example.com","content":"are you alive?"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var reply ResponseEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Content == nil || *reply.Content != "alive and well" {
		t.Fatalf("reply must be the agent envelope verbatim, got %s", rec.Body.String())
	}

	env := <-h.agentHits
	if env.ChannelType != "console" {
		t.Errorf("channelType = %q, want console", env.ChannelType)
	}
	if env.ChannelID != "/console/team-a/sup" {
		t.Errorf("channelId = %q, want /console/team-a/sup", env.ChannelID)
	}
	if env.UserID != "priya@example.com" {
		t.Errorf("userId = %q, want the console identity verbatim", env.UserID)
	}
	if env.MessageID == "" {
		t.Error("messageId must be set")
	}
	if want := SessionID("/console/team-a/sup", "priya@example.com"); env.SessionID != want {
		t.Errorf("sessionId = %q, want the deterministic derivation %q", env.SessionID, want)
	}
	if env.Content != "are you alive?" {
		t.Errorf("content = %q", env.Content)
	}
	if env.Attachments == nil || len(env.Attachments) != 0 || env.Metadata == nil || len(env.Metadata) != 0 {
		t.Error("attachments and metadata must be empty, not absent")
	}

	// A successful delivery is agent traffic for idle detection.
	snap := h.server.Activity.Snapshot("team-a")
	src, ok := snap.Agents["sup"]
	if !ok || src.GatewayTraffic == nil {
		t.Error("test-chat delivery must record gatewayTraffic activity")
	}

	// The delivery is metered under its own channel_type.
	if got := testutil.ToFloat64(h.server.Metrics.channelMsgs.WithLabelValues("console", "team-a", "delivered")); got != 1 {
		t.Errorf("kaalm_channel_messages_total{console,team-a,delivered} = %v, want 1", got)
	}

	// No channel health record: /console/... traffic is invisible to
	// GET /v1/channels/health.
	health := h.server.ChannelHealth.Snapshot("team-a")
	if len(health.Channels) != 0 {
		t.Errorf("test-chat must not enter channel health, got %d records", len(health.Channels))
	}
}

func TestTestChat_SessionIsDeterministicPerUserAndAgent(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	h.seedChannel("sync")

	body := `{"namespace":"team-a","agent":"sup","userId":"priya@example.com","content":"one"}`
	postTestChat(h, body)
	postTestChat(h, body)
	first, second := <-h.agentHits, <-h.agentHits
	if first.SessionID != second.SessionID {
		t.Error("repeated test chats from the same person must share one session")
	}
	if first.MessageID == second.MessageID {
		t.Error("each test chat must carry a fresh messageId")
	}

	postTestChat(h, `{"namespace":"team-a","agent":"sup","userId":"dev@example.com","content":"one"}`)
	other := <-h.agentHits
	if other.SessionID == first.SessionID {
		t.Error("a different person must get a different session")
	}
}

func TestTestChat_Validation(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	h.seedChannel("sync")

	cases := []struct {
		name string
		body string
		want int
	}{
		{"invalid JSON", `not json`, 400},
		{"missing agent", `{"namespace":"team-a","userId":"u","content":"c"}`, 400},
		{"missing userId", `{"namespace":"team-a","agent":"sup","content":"c"}`, 400},
		{"missing content", `{"namespace":"team-a","agent":"sup","userId":"u"}`, 400},
		{"unknown agent", `{"namespace":"team-a","agent":"nope","userId":"u","content":"c"}`, 404},
		{"unknown namespace", `{"namespace":"team-b","agent":"sup","userId":"u","content":"c"}`, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := postTestChat(h, c.body); rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}

	// GET is rejected before any body handling.
	req := httptest.NewRequest(http.MethodGet, "/v1/test-chat", nil)
	rec := httptest.NewRecorder()
	h.server.handleTestChat(rec, req)
	if rec.Code != 400 {
		t.Errorf("GET status = %d, want 400", rec.Code)
	}
}

func TestTestChat_SyncErrorMapping(t *testing.T) {
	// Delivery failure: the agent always errors, mapped to 502 delivery_failed.
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	})
	h.seedChannel("sync")
	rec := postTestChat(h, `{"namespace":"team-a","agent":"sup","userId":"u","content":"c"}`)
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Type != errDeliveryFailed {
		t.Errorf("error type = %q, want %q", body.Error.Type, errDeliveryFailed)
	}

	// Hibernated agent with no activator: 504 controller_unavailable.
	h.store.agents["team-a/sup"].Status.Phase = kaalmv1alpha1.AgentHibernated
	rec = postTestChat(h, `{"namespace":"team-a","agent":"sup","userId":"u","content":"c"}`)
	if rec.Code != 504 {
		t.Errorf("hibernated with no activator: status = %d, want 504", rec.Code)
	}
}

func TestTestChat_WakesHibernatedAgent(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"awake"}`))
	})
	h.seedChannel("sync")
	h.store.agents["team-a/sup"].Status.Phase = kaalmv1alpha1.AgentHibernated
	act := &fakeActivator{}
	h.server.Activator = act
	reg := prometheus.NewRegistry()
	h.server.Metrics = NewMetrics(reg)

	rec := postTestChat(h, `{"namespace":"team-a","agent":"sup","userId":"u","content":"c"}`)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("awake")) {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(act.calls) != 1 || act.calls[0] != "team-a/sup" {
		t.Fatalf("wake calls = %v, want one for team-a/sup", act.calls)
	}
	if got := testutil.ToFloat64(h.server.Metrics.channelWake.WithLabelValues("team-a")); got != 1 {
		t.Errorf("kaalm_channel_wake_total = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(h.server.Metrics.channelWakeDur); n == 0 {
		t.Error("wake duration histogram recorded nothing")
	}
}
