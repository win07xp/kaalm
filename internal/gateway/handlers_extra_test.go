/*
Copyright 2026.

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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"k8s.io/apimachinery/pkg/runtime"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestTaskComplete_BadRequestsAndPatchFailure(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	completions := newFakeCompletions()
	h.server.Completions = completions
	seedTask(h, "fix-42", func(task *agentryv1alpha1.AgentTask) { task.Spec.Artifacts = nil })
	cert := h.ca.issue(t, "fix-42.team-a.task.agentry.io")
	client := h.client(&cert)

	// Non-JSON body: 400.
	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/task/complete"), strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("non-JSON body = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Invalid status value: 400.
	resp = postJSON(t, client, h.url("/v1/task/complete"), map[string]any{"status": "weird"}, nil)
	if resp.StatusCode != 400 || !bodyContains(t, resp, "failure") {
		t.Errorf("invalid status = %d", resp.StatusCode)
	}

	// Total-size cap: many small artifacts that are individually fine but
	// together exceed 32 KiB. Declare them so validation passes.
	big := map[string]string{}
	declared := []agentryv1alpha1.AgentTaskArtifact{}
	for i := 0; i < 10; i++ {
		name := string(rune('a'+i)) + "-art"
		big[name] = strings.Repeat("y", 4<<10)
		declared = append(declared, agentryv1alpha1.AgentTaskArtifact{Name: name})
	}
	seedTask(h, "big-total", func(task *agentryv1alpha1.AgentTask) { task.Spec.Artifacts = declared })
	certBig := h.ca.issue(t, "big-total.team-a.task.agentry.io")
	resp = postJSON(t, h.client(&certBig), h.url("/v1/task/complete"),
		map[string]any{"status": CompletionStatusSuccess, "artifacts": big}, nil)
	if resp.StatusCode != 413 || !bodyContains(t, resp, "combined") {
		t.Errorf("total-size cap = %d, want 413", resp.StatusCode)
	}

	// Mailbox patch failure: 503.
	completions.fail = true
	resp = postJSON(t, client, h.url("/v1/task/complete"), map[string]any{"status": CompletionStatusSuccess}, nil)
	if resp.StatusCode != 503 {
		t.Errorf("patch failure = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestTaskComplete_NoTaskBacksCaller(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.server.Completions = newFakeCompletions()
	// No task seeded for this SAN.
	cert := h.ca.issue(t, "ghost.team-a.task.agentry.io")
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	resp := postJSON(t, h.client(&cert), h.url("/v1/task/complete"), map[string]any{"status": CompletionStatusSuccess}, nil)
	if resp.StatusCode != 403 || !bodyContains(t, resp, "no AgentTask") {
		t.Errorf("no backing task = %d", resp.StatusCode)
	}
}

func TestWebhook_TerminatingAndAgentMissing(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	ch := h.seedChannel("sync")

	// A Terminating channel accepts no new work: 401 (no existence leak).
	ch.Status.Phase = agentryv1alpha1.ChannelTerminating
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != 401 {
		t.Errorf("terminating channel = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Restore the channel but drop its Agent: referenced Agent not found -> 502.
	ch.Status.Phase = agentryv1alpha1.ChannelActive
	delete(h.store.agents, "team-a/sup")
	resp = h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("missing agent = %d, want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestWebhook_WrongMethod(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	req, _ := http.NewRequest(http.MethodGet, h.userSrv.URL+"/channels/team-a/support", nil)
	resp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("GET webhook = %d, want 401", resp.StatusCode)
	}
	// Unknown path on the user listener: 401.
	req, _ = http.NewRequest(http.MethodGet, h.userSrv.URL+"/nonsense", nil)
	resp, _ = h.userSrv.Client().Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("unknown user path = %d, want 401", resp.StatusCode)
	}
}

func TestWebhook_RawBodyInvalidUTF8(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync") // no content extractor -> raw-body fallback
	// A body with an invalid UTF-8 byte fails raw-body normalization with 400.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte{0xff, 0xfe})
	if resp.StatusCode != 400 || !bodyContains(t, resp, "UTF-8") {
		t.Errorf("invalid UTF-8 raw body = %d", resp.StatusCode)
	}
}

// cmErrClientset returns a fake clientset that fails the given verb on
// configmaps, to exercise the error branches.
func cmErrClientset(verb string) *k8sfake.Clientset {
	c := k8sfake.NewSimpleClientset()
	c.PrependReactor(verb, "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected failure")
	})
	return c
}

func TestKubeAsyncRecords_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	// CountPending list error.
	recs := &KubeAsyncRecords{Client: cmErrClientset("list"), OperatorNamespace: "agentry-system"}
	if _, err := recs.CountPending(ctx, "team-a", "ch"); err == nil {
		t.Error("CountPending must surface list errors")
	}

	// Patch error.
	recsP := &KubeAsyncRecords{Client: cmErrClientset("patch"), OperatorNamespace: "agentry-system"}
	if err := recsP.Patch(ctx, "req-1", []byte(`{}`)); err == nil {
		t.Error("Patch must surface errors")
	}

	// Get with a non-NotFound error.
	recsG := &KubeAsyncRecords{Client: cmErrClientset("get"), OperatorNamespace: "agentry-system"}
	if _, _, err := recsG.Get(ctx, "req-1"); err == nil {
		t.Error("Get must surface non-NotFound errors")
	}

	// PatchMailbox error.
	w := &KubeCompletionWriter{Client: cmErrClientset("patch")}
	if err := w.PatchMailbox(ctx, "team-a", "fix", map[string]string{"status": CompletionStatusSuccess}); err == nil {
		t.Error("PatchMailbox must surface errors")
	}
}

func TestHandleAsyncAccept_PendingCap(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ch := h.seedChannel("async")
	ch.Spec.Webhook.MaxPendingAsyncResponses = 1
	// Pre-fill the pending count to the cap.
	_ = h.async.Create(context.Background(), "pre-1", ch, metav1.Now().Time)

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("at-cap async = %d, want 503", resp.StatusCode)
	}
	if !bytes.Contains([]byte(resp.Header.Get("Content-Type")), []byte("json")) {
		t.Log("content-type:", resp.Header.Get("Content-Type"))
	}
	_ = resp.Body.Close()
}

func TestChannelsHealthEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	// A failure-only channel and a success channel exercise both Snapshot arms.
	h.server.ChannelHealth.RecordFailure("/channels/team-a/failing", healthReasonAuthFailed, "bad token")
	h.server.ChannelHealth.RecordSuccess("/channels/team-a/ok")

	controllerCert := h.ca.issue(t, "agentry-controller.agentry-system.svc.cluster.local")
	resp, err := h.client(&controllerCert).Get(h.url("/v1/channels/health?namespace=team-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("channels health = %d", resp.StatusCode)
	}
	var payload struct {
		WindowSeconds int `json:"windowSeconds"`
		Channels      map[string]struct {
			State     string  `json:"state"`
			Reason    *string `json:"reason"`
			LastError *string `json:"lastError"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channels["/channels/team-a/failing"].State != "failure" {
		t.Errorf("failing channel state = %q", payload.Channels["/channels/team-a/failing"].State)
	}
	// A healthy channel reports the WebhookReady reason (the success indicator).
	okReason := payload.Channels["/channels/team-a/ok"].Reason
	if okReason == nil || *okReason != healthReasonWebhookReady {
		t.Errorf("ok channel reason = %v", okReason)
	}
}

func TestLLMPaths_BearerErrorBranches(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	// A plain Deployment pod in team-b (not Agentry-managed) so the precheck
	// passes and the TokenReview path runs.
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-b"}}

	// TokenReview transport error: 503.
	h.reviewer.err = errors.New("apiserver down")
	resp := postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("TokenReview error = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Token rejected (authenticated=false): 401.
	h.reviewer.err = nil
	h.reviewer.authenticated = false
	resp = postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("rejected token = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// No credential at all: 401.
	resp = postJSON(t, h.client(nil), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
