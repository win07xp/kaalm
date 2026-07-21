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

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- channel health reduction ----

// fakeChannelHealth serves canned per-replica channel health.
type fakeChannelHealth struct {
	reachable []ReplicaChannelHealth
	total     int
	err       error
}

func (f fakeChannelHealth) NamespaceChannelHealth(
	context.Context, string,
) ([]ReplicaChannelHealth, int, error) {
	return f.reachable, f.total, f.err
}

func strptr(s string) *string { return &s }

// chTestPath is the webhook path shared across the channel-health unit tests.
const chTestPath = "/channels/default/x"

// testStorageClass is a reusable PVC storage-class name for the builder tests.
const testStorageClass = "fast"

func newChannelAt() *agentryv1alpha1.AgentChannel {
	return &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: agentryv1alpha1.AgentChannelSpec{
			Webhook: agentryv1alpha1.AgentChannelWebhook{Path: chTestPath},
		},
	}
}

func platformCond(ch *agentryv1alpha1.AgentChannel) *metav1.Condition {
	return condition(ch.Status.Conditions, agentryv1alpha1.ConditionPlatformConnected)
}

func TestReduceChannelHealth_NoDataPreservesCondition(t *testing.T) {
	// total==0 is rule 4: no condition written.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{total: 0}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatalf("rule 4: no PlatformConnected condition expected, got %+v", platformCond(ch))
	}

	// err also preserves.
	r = &AgentChannelReconciler{Health: fakeChannelHealth{
		reachable: []ReplicaChannelHealth{{}}, total: 1, err: errString("boom"),
	}}
	ch = newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("error result must preserve the existing condition")
	}
}

func TestReduceChannelHealth_Success(t *testing.T) {
	path := chTestPath
	// Two success replicas with different timestamps: the newer wins (newerHealth).
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 2,
		reachable: []ReplicaChannelHealth{
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-01-01T00:00:00Z")}}},
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-02-01T00:00:00Z")}}},
		},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != agentryv1alpha1.ReasonWebhookReady {
		t.Fatalf("rule 1 success expected True/WebhookReady, got %+v", c)
	}
}

func TestReduceChannelHealth_Failure(t *testing.T) {
	path := chTestPath
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			Channels: map[string]ChannelHealthState{path: {
				State:     healthStateFailure,
				Reason:    strptr("Timeout"),
				LastError: strptr("dial tcp: i/o timeout"),
				Timestamp: strptr("2026-01-01T00:00:00Z"),
			}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Timeout" ||
		c.Message != "dial tcp: i/o timeout" {
		t.Fatalf("rule 2 failure expected False/Timeout, got %+v", c)
	}
}

func TestReduceChannelHealth_NoRecentTraffic(t *testing.T) {
	path := chTestPath
	// A replica up longer than its window, with only an empty state: rule 3.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now().Add(-time.Hour),
			WindowSeconds: 60,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != agentryv1alpha1.ReasonNoRecentTraffic {
		t.Fatalf("rule 3 expected Unknown/NoRecentTraffic, got %+v", c)
	}
}

func TestReduceChannelHealth_DefaultReturnsNoCondition(t *testing.T) {
	path := chTestPath
	// Window not yet full and all empty: rule 4 default -> no condition.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now(),
			WindowSeconds: 3600,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("rule 4 default must leave the condition unset")
	}
}

func TestNewerHealth(t *testing.T) {
	cases := []struct {
		name string
		a, b ChannelHealthState
		want bool
	}{
		{"both nil", ChannelHealthState{}, ChannelHealthState{}, true},
		{"a nil b set", ChannelHealthState{}, ChannelHealthState{Timestamp: strptr("x")}, false},
		{"a set b nil", ChannelHealthState{Timestamp: strptr("x")}, ChannelHealthState{}, true},
		{"a newer", ChannelHealthState{Timestamp: strptr("2026-02")}, ChannelHealthState{Timestamp: strptr("2026-01")}, true},
		{"a older", ChannelHealthState{Timestamp: strptr("2026-01")}, ChannelHealthState{Timestamp: strptr("2026-02")}, false},
	}
	for _, c := range cases {
		if got := newerHealth(c.a, c.b); got != c.want {
			t.Errorf("%s: newerHealth = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDeref(t *testing.T) {
	if got := deref(strptr("value"), "fb"); got != "value" {
		t.Errorf("deref(non-empty) = %q, want value", got)
	}
	if got := deref(nil, "fb"); got != "fb" {
		t.Errorf("deref(nil) = %q, want fb", got)
	}
	if got := deref(strptr(""), "fb"); got != "fb" {
		t.Errorf("deref(empty) = %q, want fb", got)
	}
}

// ---- pure builders ----

func TestDesiredTaskPVC(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-1", Namespace: "team-a"}}
	sc := testStorageClass
	class := &agentryv1alpha1.AgentClass{
		Spec: agentryv1alpha1.AgentClassSpec{
			Persistence: agentryv1alpha1.AgentClassPersistence{StorageClassName: &sc},
		},
	}
	// Zero size defaults to 1Gi.
	pvc := desiredTaskPVC(task, class, effectiveTaskSpec{PVCSizeGi: 0})
	if pvc.Name != "fix-1-workspace" || pvc.Namespace != "team-a" {
		t.Errorf("naming wrong: %s/%s", pvc.Namespace, pvc.Name)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("access mode wrong: %v", pvc.Spec.AccessModes)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != testStorageClass {
		t.Errorf("storage class not propagated: %v", pvc.Spec.StorageClassName)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("default size not 1Gi: %s", q.String())
	}
	// A set size is honored.
	pvc = desiredTaskPVC(task, class, effectiveTaskSpec{PVCSizeGi: 5})
	q = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("size not honored: %s", q.String())
	}
}

func TestDesiredPVC_ZeroSizeDefaults(t *testing.T) {
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	class := &agentryv1alpha1.AgentClass{}
	pvc := desiredPVC(agent, class, effectiveAgentSpec{PVCSizeGi: 0})
	if pvc.Name != "sup-memory" {
		t.Errorf("pvc name wrong: %s", pvc.Name)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("zero size must default to 1Gi, got %s", q.String())
	}
}

// ---- activity merge ----

func TestMergedActivity_Sources(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	t1 := time.Now().Add(-time.Hour)
	newer := time.Now().Add(-time.Minute)
	reachable := []ReplicaActivity{
		{Agents: map[string]AgentActivity{"a": {GatewayTraffic: &t0, Heartbeat: &t1}}},
		{Agents: map[string]AgentActivity{"a": {GatewayTraffic: &newer}}},
	}
	// gatewayTraffic (default) picks the most recent traffic across replicas.
	if got := mergedActivity(reachable, "a", "gatewayTraffic"); got == nil || !got.Equal(newer) {
		t.Errorf("gatewayTraffic merge wrong: %v", got)
	}
	// agentHeartbeat ignores traffic.
	if got := mergedActivity(reachable, "a", "agentHeartbeat"); got == nil || !got.Equal(t1) {
		t.Errorf("agentHeartbeat merge wrong: %v", got)
	}
	// both takes the newer of traffic and heartbeat.
	if got := mergedActivity(reachable, "a", "both"); got == nil || !got.Equal(newer) {
		t.Errorf("both merge wrong: %v", got)
	}
	// unknown agent -> nil.
	if got := mergedActivity(reachable, "missing", "both"); got != nil {
		t.Errorf("missing agent must merge to nil, got %v", got)
	}
}

// ---- small helpers ----

func TestEqualStrings(t *testing.T) {
	if !equalStrings([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("equal slices must compare equal")
	}
	if equalStrings([]string{"a"}, []string{"a", "b"}) {
		t.Error("different lengths must not be equal")
	}
	if equalStrings([]string{"a", "x"}, []string{"a", "b"}) {
		t.Error("differing elements must not be equal")
	}
}

func TestAuthSecretNames_DedupAndHMAC(t *testing.T) {
	cb := "https://example.com/hook"
	ch := &agentryv1alpha1.AgentChannel{
		Spec: agentryv1alpha1.AgentChannelSpec{
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Auth: agentryv1alpha1.ChannelAuth{
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "inbound", Key: "t"},
					HMAC:      &agentryv1alpha1.ChannelHMAC{SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-sec", Key: "s"}},
				},
				CallbackURL: &cb,
				CallbackAuth: &agentryv1alpha1.ChannelAuth{
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "inbound", Key: "t"}, // duplicate name
				},
			},
		},
	}
	names := authSecretNames(ch)
	// Sorted, deduped: hmac-sec, inbound.
	if len(names) != 2 || names[0] != "hmac-sec" || names[1] != "inbound" {
		t.Errorf("authSecretNames = %v, want [hmac-sec inbound]", names)
	}
}

func TestPodExitMessage(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
	}}}}
	if got := podExitMessage(pod); got != "container agent exited 3" {
		t.Errorf("podExitMessage = %q", got)
	}
	// No terminated container -> fallback.
	empty := &corev1.Pod{}
	if got := podExitMessage(empty); got != "task Pod failed without a container exit code" {
		t.Errorf("podExitMessage fallback = %q", got)
	}
}

func TestNeedLeaderElection(t *testing.T) {
	if (&ActivatorServer{}).NeedLeaderElection() {
		t.Error("the activator must run on every replica (NeedLeaderElection=false)")
	}
}

// ---- cost helpers ----

func TestCheapestModel(t *testing.T) {
	mp := &agentryv1alpha1.ModelProvider{Spec: agentryv1alpha1.ModelProviderSpec{
		Models: []agentryv1alpha1.ModelProviderModel{
			{ID: "bad", CostPer1MInputTokens: "nope", CostPer1MOutputTokens: "x"},
			{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"},
			{ID: "pricey", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"},
		},
	}}
	got, ok := cheapestModel(mp)
	if !ok || got != "cheap" {
		t.Errorf("cheapestModel = %q, %v; want cheap,true", got, ok)
	}
	// No parseable costs -> ok false.
	none := &agentryv1alpha1.ModelProvider{Spec: agentryv1alpha1.ModelProviderSpec{
		Models: []agentryv1alpha1.ModelProviderModel{{ID: "m", CostPer1MInputTokens: "x", CostPer1MOutputTokens: "y"}},
	}}
	if _, ok := cheapestModel(none); ok {
		t.Error("unparseable costs must yield ok=false")
	}
}

func TestCostSanity_WarnsWhenNotCheapest(t *testing.T) {
	rec := record.NewFakeRecorder(4)
	r := &ModelProviderReconciler{Recorder: rec}
	to := "pricey"
	mp := &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "mp"},
		Spec: agentryv1alpha1.ModelProviderSpec{
			Models: []agentryv1alpha1.ModelProviderModel{
				{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"},
				{ID: "pricey", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"},
			},
			Budget: agentryv1alpha1.ModelProviderBudget{
				Policies: []agentryv1alpha1.ModelProviderBudgetPolicy{
					{AtPercent: 100, Action: "degrade", DegradeTo: &to},
				},
			},
		},
	}
	r.costSanity(mp)
	select {
	case ev := <-rec.Events:
		if ev == "" {
			t.Error("expected a non-empty warning event")
		}
	default:
		t.Error("costSanity must warn when the degrade target is not the cheapest model")
	}

	// Degrading to the cheapest emits nothing.
	rec2 := record.NewFakeRecorder(4)
	r2 := &ModelProviderReconciler{Recorder: rec2}
	cheap := "cheap"
	mp.Spec.Budget.Policies[0].DegradeTo = &cheap
	r2.costSanity(mp)
	select {
	case <-rec2.Events:
		t.Error("no warning expected when degrading to the cheapest model")
	default:
	}
}
