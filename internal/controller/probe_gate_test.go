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

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// testClock is a pinned clock a test moves forward by hand.
type testClock struct{ t time.Time }

func newTestClock() *testClock                   { return &testClock{t: time.Now()} }
func (c *testClock) now() time.Time              { return c.t }
func (c *testClock) advance(d time.Duration)     { c.t = c.t.Add(d) }
func (c *testClock) advancePast(res ctrl.Result) { c.advance(res.RequeueAfter + time.Second) }

func TestProbeGate_CachedOnlyForSameInputsBeforeDue(t *testing.T) {
	now := time.Now()
	obj := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "u1", Generation: 1}}
	key := newProbeKey(obj, "sk-1")
	var g probeGate[int]
	if _, _, ok := g.cached("p", key, now); ok {
		t.Fatal("an empty gate returned a record")
	}
	g.record("p", key, 7, now.Add(time.Minute))
	if res, wait, ok := g.cached("p", key, now.Add(10*time.Second)); !ok || res != 7 || wait != 50*time.Second {
		t.Fatalf("cached = %v, %v, %v; want 7, 50s, true", res, wait, ok)
	}
	if _, _, ok := g.cached("other", key, now); ok {
		t.Error("an unknown name returned a record")
	}
	if _, _, ok := g.cached("p", key, now.Add(time.Minute)); ok {
		t.Error("a record at its due time is still cached")
	}
	changed := []struct {
		name string
		key  probeKey
	}{
		{"uid", newProbeKey(&kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{UID: "u2", Generation: 1}}, "sk-1")},
		{"generation", newProbeKey(&kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{UID: "u1", Generation: 2}}, "sk-1")},
		{"credential", newProbeKey(obj, "sk-2")},
	}
	for _, c := range changed {
		if _, _, ok := g.cached("p", c.key, now); ok {
			t.Errorf("a changed %s still returned the record", c.name)
		}
	}
	g.forget("p")
	if _, _, ok := g.cached("p", key, now); ok {
		t.Error("forget kept the record")
	}
}

// gatedProvider is a ModelProvider with the probe on at a 60 s interval.
func gatedProvider(name string) *kaalmv1beta1.ModelProvider {
	return eventsProvider(name, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: true, IntervalSeconds: 60}
	})
}

// gatedProviderReconciler builds a ModelProviderReconciler on a pinned clock
// over a fake client holding objs, counting its status writes.
func gatedProviderReconciler(
	t *testing.T, health ProviderHealthChecker, objs ...client.Object,
) (*ModelProviderReconciler, *testClock, *statusWrites, *record.FakeRecorder) {
	t.Helper()
	writes := &statusWrites{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.ModelProvider{}).
		WithInterceptorFuncs(writes.funcs()).
		Build()
	clock := newTestClock()
	rec := record.NewFakeRecorder(32)
	return &ModelProviderReconciler{
		Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: health,
		Clock: clock.now,
	}, clock, writes, rec
}

func mustReconcile(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Only the pass whose probe is due dials the upstream. A pass in between
// keeps the recorded result, requeues for the time left, and writes nothing.
func TestModelProvider_ProbeRunsOnlyWhenDue(t *testing.T) {
	health := newFakeHealth()
	r, clock, writes, _ := gatedProviderReconciler(t, health, gatedProvider("pg-due"), providerKey("pg-due"))

	res := mustReconcile(t, r, "pg-due")
	if health.count("pg-due") != 1 || res.RequeueAfter != time.Minute {
		t.Fatalf("first pass: probes = %d, RequeueAfter = %v; want 1 and 1m", health.count("pg-due"), res.RequeueAfter)
	}
	before := writes.count()

	clock.advance(10 * time.Second)
	res = mustReconcile(t, r, "pg-due")
	if n := health.count("pg-due"); n != 1 {
		t.Errorf("a pass 10s into the interval probed again: probes = %d, want 1", n)
	}
	if res.RequeueAfter != 50*time.Second {
		t.Errorf("a pass 10s into the interval: RequeueAfter = %v, want the 50s left", res.RequeueAfter)
	}
	if n := writes.count() - before; n != 0 {
		t.Errorf("a pass between probes made %d status writes, want 0", n)
	}
	var got kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), types.NamespacedName{Name: "pg-due"}, &got); err != nil {
		t.Fatal(err)
	}
	if h := condition(got.Status.Conditions, kaalmv1beta1.ConditionHealthy); h == nil || h.Status != metav1.ConditionTrue {
		t.Errorf("Healthy = %+v, want True from the recorded probe", h)
	}

	clock.advance(51 * time.Second)
	mustReconcile(t, r, "pg-due")
	if n := health.count("pg-due"); n != 2 {
		t.Errorf("the pass after the interval: probes = %d, want 2", n)
	}
}

// A spec edit, a new credential value, or a pass that ended before the probe
// makes the next pass probe at once, inside the interval.
func TestModelProvider_SpecOrCredentialChangeProbesAtOnce(t *testing.T) {
	health := newFakeHealth()
	r, clock, _, _ := gatedProviderReconciler(t, health, gatedProvider("pg-chg"), providerKey("pg-chg"))
	key := types.NamespacedName{Name: "pg-chg"}
	probes := 0
	expect := func(step string, probed bool) {
		t.Helper()
		clock.advance(time.Second)
		mustReconcile(t, r, "pg-chg")
		if probed {
			probes++
		}
		if got := health.count("pg-chg"); got != probes {
			t.Fatalf("%s: probes = %d, want %d", step, got, probes)
		}
	}
	expect("first pass", true)
	expect("unchanged pass", false)

	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), key, &mp); err != nil {
		t.Fatal(err)
	}
	mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: "m2"})
	mp.Generation++ // the fake client does not bump it on a spec write
	if err := r.Update(ctxT(), &mp); err != nil {
		t.Fatal(err)
	}
	expect("after a spec edit", true)
	expect("unchanged pass after the edit", false)

	var sec corev1.Secret
	if err := r.Get(ctxT(), types.NamespacedName{Namespace: testOperatorNamespace, Name: "pg-chg-key"}, &sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["token"] = []byte("sk-rotated")
	if err := r.Update(ctxT(), &sec); err != nil {
		t.Fatal(err)
	}
	expect("after a credential rotation", true)
	expect("unchanged pass after the rotation", false)

	if err := r.Delete(ctxT(), &sec); err != nil {
		t.Fatal(err)
	}
	expect("Secret missing", false)
	restored := providerKey("pg-chg")
	restored.Data["token"] = []byte("sk-rotated")
	if err := r.Create(ctxT(), restored); err != nil {
		t.Fatal(err)
	}
	expect("Secret restored with the same value", true)
}

// ProviderUnhealthy is an occurrence of a failing probe: a pass between
// probes keeps Healthy=False and sends no event, and the next due probe
// sends one.
func TestModelProvider_CachedFailureSendsNoEvent(t *testing.T) {
	health := newFakeHealth()
	health.set("pg-fail", ProviderProbeResult{Err: errString("upstream 503")})
	r, clock, _, rec := gatedProviderReconciler(t, health, gatedProvider("pg-fail"), providerKey("pg-fail"))
	prefix := "Warning " + kaalmv1beta1.ReasonProviderUnhealthy

	res := mustReconcile(t, r, "pg-fail")
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 1 {
		t.Fatalf("first failing probe sent %d ProviderUnhealthy events, want 1", len(got))
	}
	clock.advance(res.RequeueAfter / 2)
	mustReconcile(t, r, "pg-fail")
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 0 {
		t.Errorf("a pass inside the backoff sent %d ProviderUnhealthy events, want 0", len(got))
	}
	var got kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), types.NamespacedName{Name: "pg-fail"}, &got); err != nil {
		t.Fatal(err)
	}
	if h := condition(got.Status.Conditions, kaalmv1beta1.ConditionHealthy); h == nil ||
		h.Status != metav1.ConditionFalse || h.Reason != kaalmv1beta1.ReasonProviderUnhealthy {
		t.Errorf("Healthy = %+v, want False/ProviderUnhealthy kept from the probe", h)
	}
	clock.advancePast(res)
	mustReconcile(t, r, "pg-fail")
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 1 {
		t.Errorf("the next due probe sent %d ProviderUnhealthy events, want 1", len(got))
	}
	if n := health.count("pg-fail"); n != 2 {
		t.Errorf("probes = %d, want 2", n)
	}
}

// The ToolProvider gates its probe the same way, and a pass between probes
// keeps the negotiated MCP revision.
func TestToolProvider_ProbeRunsOnlyWhenDue(t *testing.T) {
	tp := eventsToolProvider("tg-due")
	tp.Spec.HealthCheck.IntervalSeconds = 60
	writes := &statusWrites{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp, providerKey("tg-due")).WithStatusSubresource(tp).
		WithInterceptorFuncs(writes.funcs()).Build()
	health := newFakeToolHealth()
	health.set("tg-due", ToolProbeResult{
		ProviderProbeResult: ProviderProbeResult{Healthy: true}, MCPRevision: "2025-06-18",
	})
	clock := newTestClock()
	r := &ToolProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(10),
		OperatorNamespace: testOperatorNamespace, Health: health, Clock: clock.now,
	}

	if res := mustReconcile(t, r, "tg-due"); health.count("tg-due") != 1 || res.RequeueAfter != time.Minute {
		t.Fatalf("first pass: probes = %d, RequeueAfter = %v; want 1 and 1m", health.count("tg-due"), res.RequeueAfter)
	}
	before := writes.count()
	clock.advance(10 * time.Second)
	res := mustReconcile(t, r, "tg-due")
	if n := health.count("tg-due"); n != 1 {
		t.Errorf("a pass 10s into the interval probed again: probes = %d, want 1", n)
	}
	if res.RequeueAfter != 50*time.Second {
		t.Errorf("RequeueAfter = %v, want the 50s left", res.RequeueAfter)
	}
	if n := writes.count() - before; n != 0 {
		t.Errorf("a pass between probes made %d status writes, want 0", n)
	}
	var got kaalmv1beta1.ToolProvider
	if err := r.Get(ctxT(), types.NamespacedName{Name: "tg-due"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.MCPRevision != "2025-06-18" {
		t.Errorf("mcpRevision = %q, want the recorded 2025-06-18", got.Status.MCPRevision)
	}
	clock.advance(51 * time.Second)
	mustReconcile(t, r, "tg-due")
	if n := health.count("tg-due"); n != 2 {
		t.Errorf("the pass after the interval: probes = %d, want 2", n)
	}
}

// In the manager, the events of an Agent that names the provider (its
// create and its status writes) do not re-probe it; a spec edit does.
func TestModelProvider_CallerEventsDoNotReprobe(t *testing.T) {
	mkSecret(t, "pg-env-key")
	mkProvider(t, "pg-env", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: true, IntervalSeconds: 3600}
	})
	key := types.NamespacedName{Name: "pg-env"}
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testAPIReader.Get(ctxT(), key, &mp); err != nil {
			return err
		}
		if h := condition(mp.Status.Conditions, kaalmv1beta1.ConditionHealthy); h == nil || h.Status != metav1.ConditionTrue {
			return errString("not yet Healthy")
		}
		return nil
	})
	probes := fakeHealth.count("pg-env")

	mkAgent(t, "pg-env-caller", "pg-env-missing-class", "pg-env")
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "pg-env-caller"}, &ag); err != nil {
			return err
		}
		if len(ag.Status.Conditions) == 0 {
			return errString("the Agent has no status yet")
		}
		return nil
	})
	consistently(t, 3*time.Second, func() error {
		if n := fakeHealth.count("pg-env"); n != probes {
			return errString("a caller's events re-probed the provider")
		}
		return nil
	})

	var mp kaalmv1beta1.ModelProvider
	if err := testAPIReader.Get(ctxT(), key, &mp); err != nil {
		t.Fatal(err)
	}
	mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: "pg-env-m2"})
	if err := testClient.Update(ctxT(), &mp); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if fakeHealth.count("pg-env") <= probes {
			return errString("a spec edit did not re-probe")
		}
		return nil
	})
}
