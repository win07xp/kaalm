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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestNextPeriodStart(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) // a Sunday
	if got := nextPeriodStart("monthly", at); got != time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("monthly next = %v", got)
	}
	if got := nextPeriodStart("daily", at); got != time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) {
		t.Errorf("daily next = %v", got)
	}
	// Sunday: the next Monday is one day away.
	if got := nextPeriodStart("weekly", at); got != time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) {
		t.Errorf("weekly next (Sun) = %v", got)
	}
	// A Monday rolls to the following Monday (7 days), not the same day.
	mon := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	if got := nextPeriodStart("weekly", mon); got != time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC) {
		t.Errorf("weekly next (Mon) = %v", got)
	}
}

func TestFailClassName(t *testing.T) {
	cases := map[failClass]string{
		classConnect:   "connect_error",
		classTimeout:   "timeout",
		classBudget:    "budget_blocked",
		classUpstream:  "upstream_error",
		classNonFallle: "other",
	}
	for c, want := range cases {
		if got := failClassName(c); got != want {
			t.Errorf("failClassName(%d) = %q, want %q", c, got, want)
		}
	}
}

func TestWorkloadProviders(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.agents["team-a/sup"] = &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentSpec{
			AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "std"},
			Providers:     []agentryv1alpha1.AgentProviderReference{{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "prov"}}},
		},
	}
	fs.tasks["team-a/fix"] = &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentTaskSpec{
			AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "tstd"},
			Providers:     []agentryv1alpha1.AgentProviderReference{{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "tprov"}}},
		},
	}
	ctx := context.Background()

	refs, class, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "sup", Kind: KindAgent}})
	if !ok || class != "std" || len(refs) != 1 || refs[0].ProviderRef.Name != "prov" {
		t.Errorf("agent providers wrong: %v %q %v", refs, class, ok)
	}
	refs, class, ok = s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "fix", Kind: KindAgentTask}})
	if !ok || class != "tstd" || refs[0].ProviderRef.Name != "tprov" {
		t.Errorf("task providers wrong: %v %q %v", refs, class, ok)
	}
	// Unknown agent.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "ghost", Kind: KindAgent}}); ok {
		t.Error("unknown agent must miss")
	}
	// Unknown task.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "ghost", Kind: KindAgentTask}}); ok {
		t.Error("unknown task must miss")
	}
	// Unrecognized kind.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "x", Kind: "Weird"}}); ok {
		t.Error("unrecognized kind must miss")
	}
}

func TestIsAgentryManagedPod(t *testing.T) {
	// OwnerRef to an Agent.
	agentPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentryv1alpha1.GroupVersion.String(), Kind: "Agent"}},
	}}
	if !isAgentryManagedPod(agentPod) {
		t.Error("Agent-owned pod must be managed")
	}
	// OwnerRef to an AgentTask.
	taskPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: agentryv1alpha1.GroupVersion.String(), Kind: "AgentTask"}},
	}}
	if !isAgentryManagedPod(taskPod) {
		t.Error("AgentTask-owned pod must be managed")
	}
	// Label-based.
	labeledPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"agentry.io/workload": "agent"}}}
	if !isAgentryManagedPod(labeledPod) {
		t.Error("labeled pod must be managed")
	}
	// Plain pod: not managed. An unrelated ownerRef must not match.
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet"}},
	}}
	if isAgentryManagedPod(plain) {
		t.Error("plain pod must not be managed")
	}
}

func TestChannelSecret_Branches(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.secrets["team-a/bearer-secret/token"] = "bt"
	fs.secrets["team-a/hmac-secret/key"] = "hk"
	ctx := context.Background()

	// Bearer with secretRef resolves.
	bearer := &agentryv1alpha1.ChannelAuth{Type: authTypeBearer, SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "bearer-secret", Key: "token"}}
	if v, err := s.channelSecret(ctx, "team-a", bearer); err != nil || v != "bt" {
		t.Errorf("bearer secret = %q err=%v", v, err)
	}
	// Bearer without secretRef errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: authTypeBearer}); err == nil {
		t.Error("bearer without secretRef must error")
	}
	// HMAC with block resolves.
	hm := &agentryv1alpha1.ChannelAuth{Type: authTypeHMAC, HMAC: &agentryv1alpha1.ChannelHMAC{
		SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-secret", Key: "key"}}}
	if v, err := s.channelSecret(ctx, "team-a", hm); err != nil || v != "hk" {
		t.Errorf("hmac secret = %q err=%v", v, err)
	}
	// HMAC without block errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: authTypeHMAC}); err == nil {
		t.Error("hmac without block must error")
	}
	// Unknown type errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: "mystery"}); err == nil {
		t.Error("unknown auth type must error")
	}
}

func TestHmacHasher(t *testing.T) {
	if hmacHasher("sha1")().Size() != sha1Size {
		t.Error("sha1 hasher size wrong")
	}
	if hmacHasher("sha256")().Size() != sha256.Size {
		t.Error("sha256 hasher size wrong")
	}
	if hmacHasher("")().Size() != sha256.Size {
		t.Error("default hasher must be sha256")
	}
}

const sha1Size = 20

func TestAuthenticatePoll_HMAC(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.secrets["team-a/hmac-secret/key"] = "topsecret"

	channel := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentChannelSpec{Webhook: agentryv1alpha1.AgentChannelWebhook{
			Auth: agentryv1alpha1.ChannelAuth{Type: authTypeHMAC, HMAC: &agentryv1alpha1.ChannelHMAC{
				Header: "X-Sig", Algorithm: "sha256",
				SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-secret", Key: "key"}}},
		}},
	}
	ctx := context.Background()
	requestID := "req-9"
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)

	sign := func(id, timestamp string) string {
		mac := hmac.New(sha256.New, []byte("topsecret"))
		_, _ = fmt.Fprintf(mac, "%s\n%s", id, timestamp)
		return hex.EncodeToString(mac.Sum(nil))
	}

	// Valid signature within skew.
	good, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	good.Header.Set("X-Sig", sign(requestID, ts))
	good.Header.Set(timestampHeader, ts)
	if !s.authenticatePoll(ctx, channel, good, requestID) {
		t.Error("valid HMAC poll must pass")
	}

	// Missing/invalid timestamp.
	noTS, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	noTS.Header.Set("X-Sig", sign(requestID, ts))
	if s.authenticatePoll(ctx, channel, noTS, requestID) {
		t.Error("missing timestamp must fail")
	}

	// Timestamp outside the skew window.
	oldTS := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	skewed, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	skewed.Header.Set("X-Sig", sign(requestID, oldTS))
	skewed.Header.Set(timestampHeader, oldTS)
	if s.authenticatePoll(ctx, channel, skewed, requestID) {
		t.Error("stale timestamp must fail")
	}

	// Wrong signature.
	wrongSig, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	wrongSig.Header.Set("X-Sig", sign("other-id", ts))
	wrongSig.Header.Set(timestampHeader, ts)
	if s.authenticatePoll(ctx, channel, wrongSig, requestID) {
		t.Error("signature over the wrong requestId must fail")
	}

	// Non-hex signature.
	badHex, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	badHex.Header.Set("X-Sig", "not-hex-zz")
	badHex.Header.Set(timestampHeader, ts)
	if s.authenticatePoll(ctx, channel, badHex, requestID) {
		t.Error("non-hex signature must fail")
	}

	// Secret cannot be resolved: fail closed.
	fs2 := newFakeStore()
	s2 := &Server{Store: fs2}
	if s2.authenticatePoll(ctx, channel, good, requestID) {
		t.Error("unresolvable secret must fail")
	}
}

func TestAgentHTTPClient_WithCertFiles(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	s := &Server{Config: Config{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}}
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}

	client, err := s.agentHTTPClient(agent)
	if err != nil || client == nil {
		t.Fatalf("agentHTTPClient = %v err=%v", client, err)
	}
	// The loader is memoized: a second call reuses it without error.
	if _, err := s.agentHTTPClient(agent); err != nil {
		t.Errorf("second agentHTTPClient: %v", err)
	}
}

func TestAgentHTTPClient_BadCertFiles(t *testing.T) {
	s := &Server{Config: Config{CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key", CAFile: "/nonexistent/ca.crt"}}
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	if _, err := s.agentHTTPClient(agent); err == nil {
		t.Error("missing cert files must error")
	}
}

func TestCrossCheck(t *testing.T) {
	fs := newFakeStore()
	fs.podsByIP["10.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	a := &Authenticator{Store: fs}

	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if !a.crossCheck(r, "team-a") {
		t.Error("matching namespace must pass")
	}
	if a.crossCheck(r, "team-b") {
		t.Error("mismatched namespace must fail")
	}
	// No Pod at the source IP.
	r.RemoteAddr = "10.0.0.99:5555"
	if a.crossCheck(r, "team-a") {
		t.Error("no pod at source IP must fail")
	}
	// DisableSourceIPCheck short-circuits to true.
	a.DisableSourceIPCheck = true
	if !a.crossCheck(r, "team-a") {
		t.Error("disabled cross-check must pass")
	}
}

func TestSourceIP(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "192.168.1.5:44321"
	if got := sourceIP(r); got != "192.168.1.5" {
		t.Errorf("sourceIP = %q", got)
	}
	// No port: the raw RemoteAddr is returned.
	r.RemoteAddr = "bare-host"
	if got := sourceIP(r); got != "bare-host" {
		t.Errorf("sourceIP without port = %q", got)
	}
}
