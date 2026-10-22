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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// podWaitsInProgress reads how many Pod waits this Server holds for the
// Agent ns/name.
func podWaitsInProgress(s *Server, ns, name string) int {
	s.podWaitsMu.Lock()
	defer s.podWaitsMu.Unlock()
	return s.podWaits[agentKey{namespace: ns, name: name}]
}

// podWaitEntryExists reports whether the Server keeps a count entry for the
// Agent ns/name.
func podWaitEntryExists(s *Server, ns, name string) bool {
	s.podWaitsMu.Lock()
	defer s.podWaitsMu.Unlock()
	_, ok := s.podWaits[agentKey{namespace: ns, name: name}]
	return ok
}

// capHarness is a userHarness whose Agent team-a/sup is Provisioning with a
// Pod that never becomes reachable (nothing listens on port 1), so every
// message for it waits, and whose per-Agent cap of Pod waits is limit.
func capHarness(t *testing.T, mode string, limit int) (*userHarness, *kaalmv1beta1.AgentChannel) {
	t.Helper()
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ch := h.seedChannel(mode)
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentProvisioning
	agent.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: 10 * time.Second}
	h.server.Config.AgentServicePortOverride = 1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	h.server.Config.MaxPodWaitsPerAgent = limit
	h.server.Metrics = NewMetrics(prometheus.NewRegistry())
	return h, ch
}

// holdPodWaits starts n messages for agent that each hold a Pod wait, and
// returns once the Server counts them. Each holder has its own cancel, which
// ends its wait; t.Cleanup ends the rest and waits for every holder.
func holdPodWaits(t *testing.T, s *Server, agent *kaalmv1beta1.Agent, n int) []context.CancelFunc {
	t.Helper()
	before := podWaitsInProgress(s, agent.Namespace, agent.Name)
	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, n)
	for i := range n {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.wakeAndDeliver(ctx, "", agent, MessageEnvelope{MessageID: "hold", ChannelType: "webhook"})
		}()
	}
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
	})
	waitForPodWaits(t, s, agent, before+n)
	return cancels
}

// waitForPodWaits polls until the Server counts want Pod waits for agent.
func waitForPodWaits(t *testing.T, s *Server, agent *kaalmv1beta1.Agent, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for podWaitsInProgress(s, agent.Namespace, agent.Name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("Pod waits in progress = %d, want %d",
				podWaitsInProgress(s, agent.Namespace, agent.Name), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With the cap of waits for an Agent held, the next message fails at once as
// agent_not_ready, records AgentNotReady, and requests no wake.
func TestWakeAndDeliver_PodWaitCapRefusesTheNextMessage(t *testing.T) {
	t.Run("Provisioning", func(t *testing.T) {
		h, ch := capHarness(t, "sync", 2)
		agent := h.store.agents["team-a/sup"]
		holdPodWaits(t, h.server, agent, 2)

		start := time.Now()
		_, errType, err := h.server.wakeAndDeliver(context.Background(), ch.Spec.Path(), agent,
			MessageEnvelope{MessageID: "over", ChannelType: "webhook"})
		if errType != errAgentNotReady {
			t.Fatalf("errType = %q (%v), want %s", errType, err, errAgentNotReady)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("refusal took %s, want at once", took)
		}
		if err == nil || !strings.Contains(err.Error(), "2") {
			t.Errorf("err = %v, want it to name the cap of 2", err)
		}
		if reason, _ := healthReason(h, ch.Spec.Path()); reason != healthReasonAgentNotReady {
			t.Errorf("channel health reason = %q, want %s", reason, healthReasonAgentNotReady)
		}
		if got := testutil.ToFloat64(h.server.Metrics.channelMsgs.WithLabelValues("webhook", "team-a", errAgentNotReady)); got != 1 {
			t.Errorf("agent_not_ready messages = %v, want 1", got)
		}
		if n := podWaitsInProgress(h.server, "team-a", "sup"); n != 2 {
			t.Errorf("Pod waits in progress = %d, want the 2 holders", n)
		}
	})

	t.Run("Hibernated", func(t *testing.T) {
		h, ch := capHarness(t, "sync", 2)
		act := &fakeActivator{}
		h.server.Activator = act
		holdPodWaits(t, h.server, h.store.agents["team-a/sup"], 2)

		_, errType, err := h.server.wakeAndDeliver(context.Background(), ch.Spec.Path(),
			staleAgent(h, kaalmv1beta1.AgentHibernated, 10*time.Second),
			MessageEnvelope{MessageID: "over", ChannelType: "webhook"})
		if errType != errAgentNotReady {
			t.Fatalf("errType = %q (%v), want %s", errType, err, errAgentNotReady)
		}
		if len(act.calls) != 0 {
			t.Errorf("activator calls = %v, want none over the cap", act.calls)
		}
		if got := testutil.ToFloat64(h.server.Metrics.channelWake.WithLabelValues("team-a")); got != 0 {
			t.Errorf("wake requests = %v, want 0", got)
		}
	})
}

// The cap counts per Agent: another Agent's message gets its own wait.
func TestWakeAndDeliver_PodWaitCapIsPerAgent(t *testing.T) {
	h, _ := capHarness(t, "sync", 2)
	other := h.store.agents["team-a/sup"].DeepCopy()
	other.Name = "other"
	other.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: 80 * time.Millisecond}
	h.store.agents["team-a/other"] = other
	holdPodWaits(t, h.server, h.store.agents["team-a/sup"], 2)

	_, errType, err := h.server.wakeAndDeliver(context.Background(), "", other,
		MessageEnvelope{MessageID: "m", ChannelType: "webhook"})
	if errType != errDeliveryFailed || err == nil || !strings.Contains(err.Error(), "wakeTimeout") {
		t.Fatalf("errType = %q err = %v, want delivery_failed after its own wait", errType, err)
	}
}

// A wait frees its slot however it ends, and the count entry goes away at
// zero.
func TestWakeAndDeliver_PodWaitSlotIsFreedOnEveryOutcome(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		h, _ := capHarness(t, "sync", 1)
		agent := h.store.agents["team-a/sup"]
		cancels := holdPodWaits(t, h.server, agent, 1)
		cancels[0]()
		waitForPodWaits(t, h.server, agent, 0)

		short := agent.DeepCopy()
		short.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: 80 * time.Millisecond}
		_, errType, err := h.server.wakeAndDeliver(context.Background(), "", short,
			MessageEnvelope{MessageID: "m", ChannelType: "webhook"})
		if errType != errDeliveryFailed {
			t.Fatalf("errType = %q (%v), want delivery_failed after a wait in the freed slot", errType, err)
		}
		if podWaitEntryExists(h.server, "team-a", "sup") {
			t.Error("count entry kept after every wait ended")
		}
	})

	t.Run("wake request failed", func(t *testing.T) {
		h, _ := capHarness(t, "sync", 1)
		h.server.Activator = &fakeActivator{err: errors.New("down")}
		_, errType, _ := h.server.wakeAndDeliver(context.Background(), "",
			staleAgent(h, kaalmv1beta1.AgentHibernated, 10*time.Second),
			MessageEnvelope{MessageID: "m", ChannelType: "webhook"})
		if errType != errControllerDown {
			t.Fatalf("errType = %q, want %s", errType, errControllerDown)
		}
		if n := podWaitsInProgress(h.server, "team-a", "sup"); n != 0 {
			t.Errorf("Pod waits in progress = %d, want 0", n)
		}
		if podWaitEntryExists(h.server, "team-a", "sup") {
			t.Error("count entry kept after the wait ended")
		}
	})
}

// The wait after a refused connect during delivery takes a slot too, and is
// refused over the cap like any other.
func TestDeliverToAgent_RefusedConnectWaitCountsAgainstTheCap(t *testing.T) {
	s := deliveryServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	s.Config.AgentServicePortOverride = 1
	s.Config.AgentConnectTimeout = 20 * time.Millisecond
	s.Config.MaxPodWaitsPerAgent = 1
	agent := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Status: kaalmv1beta1.AgentStatus{
			Phase:                kaalmv1beta1.AgentRunning,
			EffectiveWakeTimeout: &metav1.Duration{Duration: 10 * time.Second},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.awaitPod(ctx, "", agent, "hold", false)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitForPodWaits(t, s, agent, 1)

	_, err := s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: "m"}, true)
	var f *podWaitFailure
	if !errors.As(err, &f) || f.errType != errAgentNotReady {
		t.Fatalf("err = %v, want a %s Pod wait failure", err, errAgentNotReady)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 1 {
		t.Errorf("connect attempts = %v, want 1", got)
	}

	const path = "/channels/team-a/support"
	_, errType, err := s.wakeAndDeliver(context.Background(), path, agent,
		MessageEnvelope{MessageID: "m2", ChannelType: "webhook"})
	if errType != errAgentNotReady {
		t.Fatalf("errType = %q (%v), want %s", errType, err, errAgentNotReady)
	}
	s.ChannelHealth.mu.Lock()
	obs := s.ChannelHealth.observations[path]
	s.ChannelHealth.mu.Unlock()
	if obs == nil || len(obs.failures) == 0 {
		t.Fatal("no channel health failure recorded")
	}
	last := obs.failures[len(obs.failures)-1]
	if last.reason != healthReasonAgentNotReady || !strings.Contains(last.lastError, "; last attempt") {
		t.Errorf("health = %s %q, want %s with the last attempt", last.reason, last.lastError, healthReasonAgentNotReady)
	}
}

// A sync message over the cap answers 503 agent_not_ready with Retry-After.
func TestWebhook_SyncAgentNotReady(t *testing.T) {
	h, _ := capHarness(t, "sync", 1)
	holdPodWaits(t, h.server, h.store.agents["team-a/sup"], 1)

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	var body map[string]errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if e := body["error"]; e.Type != errAgentNotReady || !e.Retryable {
		t.Errorf("error = %+v, want retryable %s", e, errAgentNotReady)
	}
}

// An async message over the cap is still accepted; its result carries
// agent_not_ready, retryable.
func TestWebhook_AsyncAgentNotReady(t *testing.T) {
	h, _ := capHarness(t, "async", 1)
	holdPodWaits(t, h.server, h.store.agents["team-a/sup"], 1)

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("accept = %d, want 202", resp.StatusCode)
	}
	var accept asyncAcceptResponse
	_ = decodeJSON(resp, &accept)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok, _ := h.async.Get(context.Background(), accept.RequestID)
		if ok && rec.Payload != nil {
			var payload struct {
				Error struct {
					Type      string `json:"type"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Type != errAgentNotReady || !payload.Error.Retryable {
				t.Fatalf("stored payload = %s, want retryable %s", rec.Payload, errAgentNotReady)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("error payload never stored")
}

// A platform message over the cap gets the fixed reply text.
func TestRunPlatformPipeline_AgentNotReadyReplyText(t *testing.T) {
	h, ch := capHarness(t, "async", 1)
	agent := h.store.agents["team-a/sup"]
	holdPodWaits(t, h.server, agent, 1)

	adapter := &deadlineAdapter{requests: 1}
	h.server.runPlatformPipeline(context.Background(), ch, agent, adapter, platformMessage{})

	const want = "agent_not_ready: the agent is starting and has too many messages waiting; try again shortly"
	if adapter.text != want {
		t.Errorf("reply text = %q, want %q", adapter.text, want)
	}
	if reason, _ := healthReason(h, ch.Spec.Path()); reason != healthReasonAgentNotReady {
		t.Errorf("channel health reason = %q, want %s", reason, healthReasonAgentNotReady)
	}
}

// Zero means the default cap of 100; a set value is used.
func TestMaxPodWaitsPerAgent_DefaultsTo100(t *testing.T) {
	s := &Server{}
	if got := s.maxPodWaitsPerAgent(); got != 100 {
		t.Errorf("default cap = %d, want 100", got)
	}
	s.Config.MaxPodWaitsPerAgent = 7
	if got := s.maxPodWaitsPerAgent(); got != 7 {
		t.Errorf("cap = %d, want 7", got)
	}
}
