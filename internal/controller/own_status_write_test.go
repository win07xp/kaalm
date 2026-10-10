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
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// expectOwnStatusWriteSkipped checks a provider reconciler's For() predicate
// against the events its own pass and other writers raise. reconcile runs
// one pass that writes the provider's status; obj is the provider as stored
// before it.
func expectOwnStatusWriteSkipped(
	t *testing.T, c client.Client, p predicate.Predicate, obj client.Object,
	reconcile func(), conds func(client.Object) *[]metav1.Condition,
) {
	t.Helper()
	get := func(into client.Object) client.Object {
		t.Helper()
		if err := c.Get(ctxT(), client.ObjectKeyFromObject(obj), into); err != nil {
			t.Fatal(err)
		}
		return into
	}
	fresh := func() client.Object { return obj.DeepCopyObject().(client.Object) }
	before := get(fresh())
	reconcile()
	after := get(fresh())
	if after.GetResourceVersion() == before.GetResourceVersion() {
		t.Fatal("the pass wrote no status; the test needs one that does")
	}
	if p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Error("the reconciler's own status write was admitted")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: after, ObjectNew: after}) {
		t.Error("a resync of the stored object was dropped")
	}

	foreign := after.DeepCopyObject().(client.Object)
	apimeta.SetStatusCondition(conds(foreign), metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: metav1.ConditionFalse, Reason: "EditedByHand", Message: "x",
	})
	if err := c.Status().Update(ctxT(), foreign); err != nil {
		t.Fatal(err)
	}
	if !p.Update(event.UpdateEvent{ObjectOld: after, ObjectNew: foreign}) {
		t.Error("a status write by another client was dropped")
	}

	edited := get(fresh())
	edited.SetGeneration(edited.GetGeneration() + 1) // the fake client does not bump it
	edited.SetLabels(map[string]string{"team": "a"})
	if err := c.Update(ctxT(), edited); err != nil {
		t.Fatal(err)
	}
	if !p.Update(event.UpdateEvent{ObjectOld: foreign, ObjectNew: edited}) {
		t.Error("a spec or metadata edit was dropped")
	}
	if !p.Create(event.CreateEvent{Object: after}) || !p.Delete(event.DeleteEvent{Object: after}) {
		t.Error("a create or delete was dropped")
	}
}

// A provider reconciler's For() watch drops the update event its own status
// write raises, and admits every other change to the provider.
func TestOwnStatusWrites_ModelProvider(t *testing.T) {
	mp := eventsProvider("own-write-mp", nil)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(mp, providerKey("own-write-mp")).
		WithStatusSubresource(&kaalmv1beta1.ModelProvider{}).Build()
	r := &ModelProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(16), OperatorNamespace: testOperatorNamespace,
		Health: newFakeHealth(),
	}
	expectOwnStatusWriteSkipped(t, c, r.ownWrites.skipOwn(), mp,
		func() { mustReconcile(t, r, mp.Name) },
		func(o client.Object) *[]metav1.Condition { return &o.(*kaalmv1beta1.ModelProvider).Status.Conditions })
}

func TestOwnStatusWrites_ToolProvider(t *testing.T) {
	tp := eventsToolProvider("own-write-tp")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp, providerKey("own-write-tp")).
		WithStatusSubresource(&kaalmv1beta1.ToolProvider{}).Build()
	health := newFakeToolHealth()
	health.set(tp.Name, ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true}})
	r := &ToolProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(16), OperatorNamespace: testOperatorNamespace,
		Health: health,
	}
	expectOwnStatusWriteSkipped(t, c, r.ownWrites.skipOwn(), tp,
		func() { mustReconcile(t, r, tp.Name) },
		func(o client.Object) *[]metav1.Condition { return &o.(*kaalmv1beta1.ToolProvider).Status.Conditions })
}

// A write the predicate never saw an event for leaves no record behind that
// could drop a later event, and a forgotten provider admits everything.
func TestOwnStatusWrites_RecordIsOneShot(t *testing.T) {
	var w ownWrites
	obj := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p", ResourceVersion: "5"}}
	old := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p", ResourceVersion: "4"}}
	p := w.skipOwn()
	w.record(obj)
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: obj}) {
		t.Fatal("own write admitted")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: obj}) {
		t.Error("a second event at the recorded version was dropped")
	}
	w.record(obj)
	w.forget("p")
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: obj}) {
		t.Error("an event after forget was dropped")
	}
}
