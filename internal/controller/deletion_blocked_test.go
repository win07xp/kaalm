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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

func TestDeletionBlockedMessage(t *testing.T) {
	if got, want := deletionBlockedMessage([]string{"Agent team-a/support"}),
		"deletion blocked: Agent team-a/support still references it"; got != want {
		t.Errorf("one referrer: %q, want %q", got, want)
	}
	got := deletionBlockedMessage([]string{"Agent team-a/support", "AgentTask team-a/nightly", "AgentClass standard"})
	want := "deletion blocked: 3 objects still reference it, including Agent team-a/support"
	if got != want {
		t.Errorf("three referrers: %q, want %q", got, want)
	}
}

// expectDeletionBlocked waits for Ready=False DeletionBlocked whose message
// names the referrer, then checks the Warning event fired once.
func expectDeletionBlocked(t *testing.T, kind, name string, conds func() []metav1.Condition, referrer string) {
	t.Helper()
	eventually(t, func() error {
		c := condition(conds(), kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonDeletionBlocked {
			return errString("Ready is not False/DeletionBlocked yet")
		}
		if !strings.Contains(c.Message, referrer) {
			return errString("message does not name " + referrer + ": " + c.Message)
		}
		return nil
	})
	expectEvent(t, kind, "", name, kaalmv1beta1.ReasonDeletionBlocked, corev1.EventTypeWarning, referrer)
	time.Sleep(time.Second)
	if n := eventCount(objectEvents(t, kind, "", name, kaalmv1beta1.ReasonDeletionBlocked)); n != 1 {
		t.Errorf("DeletionBlocked emitted %d times, want 1", n)
	}
}

// expectHealthyNotProbed waits for Healthy=Unknown NotProbed whose message
// contains why.
func expectHealthyNotProbed(t *testing.T, conds func() []metav1.Condition, why string) {
	t.Helper()
	eventually(t, func() error {
		c := condition(conds(), kaalmv1beta1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != kaalmv1beta1.ReasonNotProbed {
			return errString("Healthy is not Unknown/NotProbed yet")
		}
		if !strings.Contains(c.Message, why) {
			return errString("Healthy message does not contain " + why + ": " + c.Message)
		}
		return nil
	})
}

func deleteObject(t *testing.T, obj client.Object) {
	t.Helper()
	if err := testClient.Delete(ctxT(), obj); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s: %v", obj.GetName(), err)
	}
}

func expectGone(t *testing.T, key types.NamespacedName, obj client.Object) {
	t.Helper()
	eventually(t, func() error {
		if err := testAPIReader.Get(ctxT(), key, obj); apierrors.IsNotFound(err) {
			return nil
		}
		return errString(key.Name + " still exists")
	})
}

// expectClassIndexed waits until the manager's cache lists the class under
// the provider in index. The reconciler finds referrers in that cache, so a
// delete sent before the cache holds the class would be released, not held.
func expectClassIndexed(t *testing.T, index, provider, class string) {
	t.Helper()
	eventually(t, func() error {
		var list kaalmv1beta1.AgentClassList
		if err := testCache.List(ctxT(), &list, client.MatchingFields{index: provider}); err != nil {
			return err
		}
		for _, ac := range list.Items {
			if ac.Name == class {
				return nil
			}
		}
		return errString("AgentClass " + class + " not in the cache yet")
	})
}

func TestModelProvider_DeleteBlockedIsVisible(t *testing.T) {
	mkSecret(t, "db-mp-key")
	mkProvider(t, "db-mp", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "db-mp-key", Key: "token"}
	})
	mkClass(t, "db-mp-class", "db-mp")
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-mp"}, &mp); err != nil {
			return err
		}
		if len(mp.Finalizers) == 0 {
			return errString("finalizer not added yet")
		}
		return nil
	})
	expectClassIndexed(t, IndexAllowedProviders, "db-mp", "db-mp-class")

	deleteObject(t, &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-mp"}})
	expectDeletionBlocked(t, "ModelProvider", "db-mp", func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-mp"}, &mp)
		return mp.Status.Conditions
	}, "AgentClass db-mp-class")
	expectHealthyNotProbed(t, func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-mp"}, &mp)
		return mp.Status.Conditions
	}, "deletion is held")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-mp-class"}})
	expectGone(t, types.NamespacedName{Name: "db-mp"}, &kaalmv1beta1.ModelProvider{})
}

func TestToolProvider_DeleteBlockedIsVisible(t *testing.T) {
	mkToolProvider(t, "db-tp", nil)
	mkWorkloadClass(t, "db-tp-class", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "db-tp"}}
	})
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-tp"}, &tp); err != nil {
			return err
		}
		if len(tp.Finalizers) == 0 {
			return errString("finalizer not added yet")
		}
		return nil
	})
	expectClassIndexed(t, IndexAllowedToolProviders, "db-tp", "db-tp-class")

	deleteObject(t, &kaalmv1beta1.ToolProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-tp"}})
	expectDeletionBlocked(t, "ToolProvider", "db-tp", toolProviderConditions("db-tp"), "AgentClass db-tp-class")
	expectHealthyNotProbed(t, toolProviderConditions("db-tp"), "deletion is held")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-tp-class"}})
	expectGone(t, types.NamespacedName{Name: "db-tp"}, &kaalmv1beta1.ToolProvider{})
}

func TestAgentClass_DeleteBlockedIsVisible(t *testing.T) {
	mkClass(t, "db-ac")
	mkAgent(t, "db-ac-a", "db-ac")
	mkAgent(t, "db-ac-b", "db-ac")
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-ac"}, &ac); err != nil {
			return err
		}
		if ac.Status.AgentsInUse != 2 {
			return errString("agentsInUse not 2 yet")
		}
		return nil
	})

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-ac"}})
	classConds := func() []metav1.Condition {
		var ac kaalmv1beta1.AgentClass
		_ = testAPIReader.Get(ctxT(), types.NamespacedName{Name: "db-ac"}, &ac)
		return ac.Status.Conditions
	}
	expectDeletionBlocked(t, "AgentClass", "db-ac", classConds, "Agent default/db-ac-a")
	if c := condition(classConds(), kaalmv1beta1.ConditionReady); !strings.Contains(c.Message, "2 objects") {
		t.Errorf("message does not carry the referrer count: %q", c.Message)
	}

	for _, n := range []string{"db-ac-a", "db-ac-b"} {
		deleteObject(t, &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"}})
	}
	expectGone(t, types.NamespacedName{Name: "db-ac"}, &kaalmv1beta1.AgentClass{})
}

// waitFinalizer waits until the stored obj at key carries a finalizer.
func waitFinalizer(t *testing.T, key types.NamespacedName, obj client.Object) {
	t.Helper()
	eventually(t, func() error {
		if err := testAPIReader.Get(ctxT(), key, obj); err != nil {
			return err
		}
		if len(obj.GetFinalizers()) == 0 {
			return errString("finalizer not added yet")
		}
		return nil
	})
}

// cacheLagDelay is how long the cache takes in a lagged referrer: longer
// than the release pass takes, so the delete reaches the reconciler before
// the cache holds the referrer.
const cacheLagDelay = 2 * time.Second

// A ModelProvider deleted before the cache holds the AgentClass that
// allows it stays held: the release confirms with a live read.
func TestModelProvider_DeleteHeldBeforeCacheSeesReferrer(t *testing.T) {
	mkSecret(t, "db-lag-mp-key")
	mkProvider(t, "db-lag-mp", nil)
	key := types.NamespacedName{Name: "db-lag-mp"}
	waitFinalizer(t, key, &kaalmv1beta1.ModelProvider{})

	lagCacheFor(t, "AgentClass/db-lag-mp-class", cacheLagDelay)
	mkClass(t, "db-lag-mp-class", "db-lag-mp")
	deleteObject(t, &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-mp"}})
	expectDeletionBlocked(t, "ModelProvider", "db-lag-mp", func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testAPIReader.Get(ctxT(), key, &mp)
		return mp.Status.Conditions
	}, "AgentClass db-lag-mp-class")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-mp-class"}})
	expectGone(t, key, &kaalmv1beta1.ModelProvider{})
}

// A ToolProvider deleted before the cache holds the AgentClass that allows
// it stays held.
func TestToolProvider_DeleteHeldBeforeCacheSeesReferrer(t *testing.T) {
	mkToolProvider(t, "db-lag-tp", nil)
	key := types.NamespacedName{Name: "db-lag-tp"}
	waitFinalizer(t, key, &kaalmv1beta1.ToolProvider{})

	lagCacheFor(t, "AgentClass/db-lag-tp-class", cacheLagDelay)
	mkWorkloadClass(t, "db-lag-tp-class", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "db-lag-tp"}}
	})
	deleteObject(t, &kaalmv1beta1.ToolProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-tp"}})
	expectDeletionBlocked(t, "ToolProvider", "db-lag-tp", toolProviderConditions("db-lag-tp"), "AgentClass db-lag-tp-class")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-tp-class"}})
	expectGone(t, key, &kaalmv1beta1.ToolProvider{})
}

// An AgentClass deleted before the cache holds the Agent on it stays held.
func TestAgentClass_DeleteHeldBeforeCacheSeesReferrer(t *testing.T) {
	mkClass(t, "db-lag-ac")
	key := types.NamespacedName{Name: "db-lag-ac"}
	waitFinalizer(t, key, &kaalmv1beta1.AgentClass{})

	lagCacheFor(t, "Agent/default/db-lag-ac-a", cacheLagDelay)
	mkAgent(t, "db-lag-ac-a", "db-lag-ac")
	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-ac"}})
	expectDeletionBlocked(t, "AgentClass", "db-lag-ac", func() []metav1.Condition {
		var ac kaalmv1beta1.AgentClass
		_ = testAPIReader.Get(ctxT(), key, &ac)
		return ac.Status.Conditions
	}, "Agent default/db-lag-ac-a")

	deleteObject(t, &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "db-lag-ac-a", Namespace: "default"}})
	expectGone(t, key, &kaalmv1beta1.AgentClass{})
}

// heldDeleteClient is a fake client holding objs and an AgentClass that
// lists the provider name in both allowed lists, with every referrer index
// a provider delete reads.
func heldDeleteClient(t *testing.T, provider string, status client.Object, objs ...client.Object) client.Client {
	t.Helper()
	class := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "held-class"},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedProviders:     []kaalmv1beta1.LocalObjectReference{{Name: provider}},
			AllowedToolProviders: []kaalmv1beta1.LocalObjectReference{{Name: provider}},
		},
	}
	return withReferrerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).
		WithObjects(append(objs, class, status)...).WithStatusSubresource(status).
		Build()
}

// staleHoldConds are the conditions of a delete hold that began before the
// held pass set Healthy: Ready=False DeletionBlocked naming the class, and
// Healthy True from the last probe.
func staleHoldConds() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{Type: kaalmv1beta1.ConditionReady, Status: metav1.ConditionFalse, Reason: kaalmv1beta1.ReasonDeletionBlocked,
			Message: deletionBlockedMessage([]string{"AgentClass held-class"}), LastTransitionTime: now},
		{Type: kaalmv1beta1.ConditionHealthy, Status: metav1.ConditionTrue, Reason: kaalmv1beta1.ReasonUpstreamReachable,
			Message: "reachable", LastTransitionTime: now},
	}
}

// expectHeldHealthy checks Healthy=Unknown NotProbed for a held delete and
// that the pass sent no event: the hold itself is not new.
func expectHeldHealthy(t *testing.T, conds []metav1.Condition, rec *events.FakeRecorder) {
	t.Helper()
	c := condition(conds, kaalmv1beta1.ConditionHealthy)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != kaalmv1beta1.ReasonNotProbed ||
		!strings.Contains(c.Message, "deletion is held") {
		t.Errorf("Healthy = %+v, want Unknown NotProbed for a held delete", c)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("unexpected event %q", e)
	default:
	}
}

// A ToolProvider hold whose DeletionBlocked message is unchanged still
// refreshes a stale Healthy.
func TestToolProvider_HeldDeleteRefreshesStaleHealthy(t *testing.T) {
	now := metav1.Now()
	tp := eventsToolProvider("held-tp")
	tp.DeletionTimestamp = &now
	tp.Status.Conditions = staleHoldConds()
	c := heldDeleteClient(t, tp.Name, tp)
	rec := events.NewFakeRecorder(8)
	r := &ToolProviderReconciler{Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tp.Name}}); err != nil {
		t.Fatal(err)
	}
	var got kaalmv1beta1.ToolProvider
	if err := c.Get(context.Background(), types.NamespacedName{Name: tp.Name}, &got); err != nil {
		t.Fatal(err)
	}
	expectHeldHealthy(t, got.Status.Conditions, rec)
}

// A ModelProvider hold whose DeletionBlocked message is unchanged still
// refreshes a stale Healthy.
func TestModelProvider_HeldDeleteRefreshesStaleHealthy(t *testing.T) {
	now := metav1.Now()
	mp := probedProvider("held-mp", metav1.ConditionTrue, time.Now().Add(-time.Hour))
	mp.DeletionTimestamp = &now
	mp.Status.Conditions = staleHoldConds()
	c := heldDeleteClient(t, mp.Name, mp)
	rec := events.NewFakeRecorder(8)
	r := &ModelProviderReconciler{
		Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: mp.Name}}); err != nil {
		t.Fatal(err)
	}
	var got kaalmv1beta1.ModelProvider
	if err := c.Get(context.Background(), types.NamespacedName{Name: mp.Name}, &got); err != nil {
		t.Fatal(err)
	}
	expectHeldHealthy(t, got.Status.Conditions, rec)
}

// indexedReferrerClient is a fake client holding objs, with every referrer
// index registered from the same table SetupIndexers uses.
func indexedReferrerClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return withReferrerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.ModelProvider{}, &kaalmv1beta1.ToolProvider{}, &kaalmv1beta1.AgentClass{}).
		Build()
}

// allowingClass is an AgentClass that allows provider in both allowed lists.
func allowingClass(name, provider string) *kaalmv1beta1.AgentClass {
	return &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedProviders:     []kaalmv1beta1.LocalObjectReference{{Name: provider}},
			AllowedToolProviders: []kaalmv1beta1.LocalObjectReference{{Name: provider}},
		},
	}
}

// The live read and the index lookup find the same referrers, in the same
// order, for each of the three referrer sets.
func TestListReferrers_LiveMatchesIndex(t *testing.T) {
	objs := []client.Object{
		&kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ag-p"},
			Spec: kaalmv1beta1.AgentSpec{
				AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "cls"},
				Providers: []kaalmv1beta1.AgentProviderReference{
					{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "p"}},
				},
			},
		},
		&kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ag-tool"},
			Spec: kaalmv1beta1.AgentSpec{
				Tools: []kaalmv1beta1.AgentToolGrant{{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "p"}}},
			},
		},
		&kaalmv1beta1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "task-p"},
			Spec: kaalmv1beta1.AgentTaskSpec{
				AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "cls"},
				Providers: []kaalmv1beta1.AgentProviderReference{
					{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "p"}},
				},
			},
		},
		allowingClass("allow-p", "p"),
		allowingClass("allow-q", "q"),
	}
	c := indexedReferrerClient(t, objs...)
	cases := []struct {
		name string
		set  referrerIndexes
		ref  string
		want []string
	}{
		{"model provider", modelProviderReferrers, "p", []string{"Agent team-a/ag-p", "AgentTask team-b/task-p", "AgentClass allow-p"}},
		{"tool provider", toolProviderReferrers, "p", []string{"Agent team-a/ag-tool", "AgentClass allow-p"}},
		{"agent class", agentClassReferrers, "cls", []string{"Agent team-a/ag-p", "AgentTask team-b/task-p"}},
		{"no referrers", modelProviderReferrers, "unused", nil},
	}
	for _, tc := range cases {
		for _, live := range []bool{false, true} {
			got, err := listReferrers(context.Background(), c, tc.ref, tc.set, live)
			if err != nil {
				t.Fatalf("%s (live=%v): %v", tc.name, live, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("%s (live=%v) = %q, want %q", tc.name, live, got, tc.want)
			}
		}
	}
}

// An empty cached list is confirmed against the live reader, and a referrer
// only the live reader holds keeps a ModelProvider delete held.
func TestDeleteReferrers_ConfirmsLiveWhenCacheEmpty(t *testing.T) {
	name := "live-held"
	live := indexedReferrerClient(t, allowingClass("live-class", name))
	got, liveOnly, err := deleteReferrers(context.Background(), indexedReferrerClient(t), live, name, modelProviderReferrers)
	if err != nil || strings.Join(got, ",") != "AgentClass live-class" || !liveOnly {
		t.Fatalf("deleteReferrers = %q, %v, %v; want [AgentClass live-class], live only", got, liveOnly, err)
	}

	r := deleteReconciler(t, interceptor.Funcs{}, append(spendConfigMaps(name), deletingProvider(name))...)
	r.APIReader = live
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &mp); err != nil {
		t.Fatalf("provider gone after a live-only hold: %v", err)
	}
	if len(mp.Finalizers) == 0 {
		t.Error("finalizer removed although the live reader holds a referrer")
	}
	c := condition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil || c.Reason != kaalmv1beta1.ReasonDeletionBlocked || !strings.Contains(c.Message, "AgentClass live-class") {
		t.Errorf("Ready = %+v, want DeletionBlocked naming AgentClass live-class", c)
	}
	for _, cm := range []string{gateway.BudgetConfigMapName(name), gateway.AgentSpendConfigMapName(name)} {
		if !configMapExists(t, r.Client, cm) {
			t.Errorf("ConfigMap %s deleted although the delete is held", cm)
		}
	}
}

// A cached list that holds the delete settles it: no live read runs.
func TestDeleteReferrers_SkipsLiveWhenCacheHolds(t *testing.T) {
	cached := indexedReferrerClient(t, allowingClass("cached-class", "p"))
	got, liveOnly, err := deleteReferrers(context.Background(), cached, newErrListClient(t), "p", modelProviderReferrers)
	if err != nil || strings.Join(got, ",") != "AgentClass cached-class" || liveOnly {
		t.Fatalf("deleteReferrers = %q, %v, %v; want the cached list and no live read", got, liveOnly, err)
	}
}

// A live read that fails keeps the finalizer on all three resources, and a
// ModelProvider's spend ConfigMaps with it.
func TestReconcileDelete_LiveListErrorKeepsFinalizer(t *testing.T) {
	now := metav1.Now()
	ctx := context.Background()
	req := func(name string) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	}

	t.Run("ModelProvider", func(t *testing.T) {
		name := "live-err-mp"
		c := indexedReferrerClient(t, append(spendConfigMaps(name), deletingProvider(name))...)
		r := &ModelProviderReconciler{
			Client: c, APIReader: newErrListClient(t), Recorder: events.NewFakeRecorder(8),
			OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
		}
		if _, err := r.Reconcile(ctx, req(name)); err == nil {
			t.Fatal("reconcile returned nil; want the live list error")
		}
		var mp kaalmv1beta1.ModelProvider
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &mp); err != nil || len(mp.Finalizers) == 0 {
			t.Errorf("finalizer not kept (err=%v)", err)
		}
		for _, cm := range []string{gateway.BudgetConfigMapName(name), gateway.AgentSpendConfigMapName(name)} {
			if !configMapExists(t, c, cm) {
				t.Errorf("ConfigMap %s deleted after a failed live read", cm)
			}
		}
	})

	t.Run("ToolProvider", func(t *testing.T) {
		tp := eventsToolProvider("live-err-tp")
		tp.DeletionTimestamp = &now
		c := indexedReferrerClient(t, tp)
		r := &ToolProviderReconciler{
			Client: c, APIReader: newErrListClient(t), Recorder: events.NewFakeRecorder(8),
			OperatorNamespace: testOperatorNamespace,
		}
		if _, err := r.Reconcile(ctx, req(tp.Name)); err == nil {
			t.Fatal("reconcile returned nil; want the live list error")
		}
		var got kaalmv1beta1.ToolProvider
		if err := c.Get(ctx, types.NamespacedName{Name: tp.Name}, &got); err != nil || len(got.Finalizers) == 0 {
			t.Errorf("finalizer not kept (err=%v)", err)
		}
	})

	t.Run("AgentClass", func(t *testing.T) {
		ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
			Name: "live-err-ac", DeletionTimestamp: &now, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
		}}
		c := indexedReferrerClient(t, ac)
		r := &AgentClassReconciler{Client: c, APIReader: newErrListClient(t), Recorder: events.NewFakeRecorder(8)}
		if _, err := r.Reconcile(ctx, req(ac.Name)); err == nil {
			t.Fatal("reconcile returned nil; want the live list error")
		}
		var got kaalmv1beta1.AgentClass
		if err := c.Get(ctx, types.NamespacedName{Name: ac.Name}, &got); err != nil || len(got.Finalizers) == 0 {
			t.Errorf("finalizer not kept (err=%v)", err)
		}
	})
}

// A hold that only the live read shows comes back on a short timer, since no
// cache event may arrive to re-check it. A hold the cache shows waits for the
// referrer's event, as before.
func TestReconcileDelete_LiveOnlyHoldRequeuesSoon(t *testing.T) {
	now := metav1.Now()
	ctx := context.Background()
	req := func(name string) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	}
	agentOn := func(class string) *kaalmv1beta1.Agent {
		return &kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "on-" + class},
			Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: class}},
		}
	}
	// check reconciles obj with the referrer in the live reader only, then
	// in both readers, and checks the requeue of each hold.
	check := func(t *testing.T, reconcile func(cached, live client.Client) (ctrl.Result, error),
		obj func() client.Object, referrer client.Object) {
		t.Helper()
		res, err := reconcile(indexedReferrerClient(t, obj()), indexedReferrerClient(t, referrer))
		if err != nil {
			t.Fatalf("live-only hold: %v", err)
		}
		if res.RequeueAfter <= 0 || res.RequeueAfter >= time.Minute {
			t.Errorf("live-only hold RequeueAfter = %v, want a few seconds", res.RequeueAfter)
		}
		res, err = reconcile(indexedReferrerClient(t, obj(), referrer.DeepCopyObject().(client.Object)),
			indexedReferrerClient(t, referrer.DeepCopyObject().(client.Object)))
		if err != nil {
			t.Fatalf("cached hold: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Errorf("cached hold RequeueAfter = %v, want 0 (the referrer's event re-checks it)", res.RequeueAfter)
		}
	}

	t.Run("ModelProvider", func(t *testing.T) {
		name := "live-requeue-mp"
		check(t, func(cached, live client.Client) (ctrl.Result, error) {
			r := &ModelProviderReconciler{
				Client: cached, APIReader: live, Recorder: events.NewFakeRecorder(8),
				OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
			}
			return r.Reconcile(ctx, req(name))
		}, func() client.Object { return deletingProvider(name) }, allowingClass("live-requeue-mp-class", name))
	})

	t.Run("ToolProvider", func(t *testing.T) {
		name := "live-requeue-tp"
		check(t, func(cached, live client.Client) (ctrl.Result, error) {
			r := &ToolProviderReconciler{
				Client: cached, APIReader: live, Recorder: events.NewFakeRecorder(8),
				OperatorNamespace: testOperatorNamespace,
			}
			return r.Reconcile(ctx, req(name))
		}, func() client.Object {
			tp := eventsToolProvider(name)
			tp.DeletionTimestamp = &now
			return tp
		}, allowingClass("live-requeue-tp-class", name))
	})

	t.Run("AgentClass", func(t *testing.T) {
		name := "live-requeue-ac"
		check(t, func(cached, live client.Client) (ctrl.Result, error) {
			r := &AgentClassReconciler{Client: cached, APIReader: live, Recorder: events.NewFakeRecorder(8)}
			return r.Reconcile(ctx, req(name))
		}, func() client.Object {
			return &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
				Name: name, DeletionTimestamp: &now, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
			}}
		}, agentOn(name))
	})
}

// A live-only hold whose DeletionBlocked status write fails returns the error
// with an empty result: controller-runtime ignores a RequeueAfter sent with an
// error and logs a warning, and the error already requeues with backoff.
func TestReconcileDelete_HoldWriteErrorReturnsEmptyResult(t *testing.T) {
	now := metav1.Now()
	ctx := context.Background()
	req := func(name string) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	}
	failStatus := func(t *testing.T, obj client.Object) client.Client {
		t.Helper()
		return interceptor.NewClient(indexedReferrerClient(t, obj).(client.WithWatch), interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(kaalmv1beta1.GroupVersion.WithResource("status").GroupResource(), obj.GetName(), nil)
			},
		})
	}
	check := func(t *testing.T, res ctrl.Result, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("reconcile returned nil; want the status write error")
		}
		if res != (ctrl.Result{}) {
			t.Errorf("result = %+v with an error, want an empty result", res)
		}
	}

	t.Run("ModelProvider", func(t *testing.T) {
		name := "hold-err-mp"
		r := &ModelProviderReconciler{
			Client: failStatus(t, deletingProvider(name)), Recorder: events.NewFakeRecorder(8),
			APIReader:         indexedReferrerClient(t, allowingClass("hold-err-mp-class", name)),
			OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
		}
		res, err := r.Reconcile(ctx, req(name))
		check(t, res, err)
	})

	t.Run("AgentClass", func(t *testing.T) {
		name := "hold-err-ac"
		r := &AgentClassReconciler{
			Client: failStatus(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
				Name: name, DeletionTimestamp: &now, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
			}}),
			APIReader: indexedReferrerClient(t, &kaalmv1beta1.Agent{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "on-" + name},
				Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: name}},
			}),
			Recorder: events.NewFakeRecorder(8),
		}
		res, err := r.Reconcile(ctx, req(name))
		check(t, res, err)
	})
}
