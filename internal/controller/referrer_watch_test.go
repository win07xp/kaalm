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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func watchAgent(class string, providers, tools []string, podUpToDate string) *kaalmv1beta1.Agent {
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: class}},
	}
	for _, p := range providers {
		ag.Spec.Providers = append(ag.Spec.Providers,
			kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: p}})
	}
	for _, tp := range tools {
		ag.Spec.Tools = append(ag.Spec.Tools,
			kaalmv1beta1.AgentToolGrant{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: tp}})
	}
	if podUpToDate != "" {
		apimeta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: kaalmv1beta1.ConditionPodUpToDate, Status: metav1.ConditionFalse, Reason: podUpToDate,
		})
	}
	return ag
}

func watchTask(class string, providers, tools []string) *kaalmv1beta1.AgentTask {
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec:       kaalmv1beta1.AgentTaskSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: class}},
	}
	for _, p := range providers {
		task.Spec.Providers = append(task.Spec.Providers,
			kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: p}})
	}
	for _, tp := range tools {
		task.Spec.Tools = append(task.Spec.Tools,
			kaalmv1beta1.AgentToolGrant{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: tp}})
	}
	return task
}

// statusOnly returns a copy of obj whose status changed and whose spec did
// not: a phase moved and the resourceVersion with it.
func statusOnly(obj client.Object) client.Object {
	switch o := obj.(type) {
	case *kaalmv1beta1.Agent:
		c := o.DeepCopy()
		c.ResourceVersion = "2"
		c.Status.Phase = kaalmv1beta1.AgentIdle
		return c
	case *kaalmv1beta1.AgentTask:
		c := o.DeepCopy()
		c.ResourceVersion = "2"
		c.Status.Phase = kaalmv1beta1.TaskRunning
		return c
	}
	panic("unexpected type")
}

func admitsUpdate(p predicate.Predicate, old, new client.Object) bool {
	return p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: new})
}

// Each workload watch admits what its reconciler reads from the workload,
// and every create and delete; a status-only update reaches none of them.
func TestReferrerWatch_StatusOnlyUpdatesDoNotEnqueue(t *testing.T) {
	agent := watchAgent("c", []string{"mp"}, []string{"tp"}, "")
	task := watchTask("c", []string{"mp"}, []string{"tp"})
	preds := map[string]predicate.Predicate{
		"providerRefsChanged": providerRefsChanged(),
		"toolGrantsChanged":   toolGrantsChanged(),
		"classUsageChanged":   classUsageChanged(),
	}
	for name, p := range preds {
		for _, obj := range []client.Object{agent, task} {
			if admitsUpdate(p, obj, statusOnly(obj)) {
				t.Errorf("%s admitted a status-only %T update", name, obj)
			}
			if !p.Create(event.CreateEvent{Object: obj}) {
				t.Errorf("%s rejected a %T create", name, obj)
			}
			if !p.Delete(event.DeleteEvent{Object: obj}) {
				t.Errorf("%s rejected a %T delete", name, obj)
			}
			if p.Generic(event.GenericEvent{Object: obj}) {
				t.Errorf("%s admitted a generic %T event", name, obj)
			}
		}
	}
}

func TestReferrerWatch_ReferenceChangesEnqueue(t *testing.T) {
	cases := []struct {
		name     string
		pred     predicate.Predicate
		old, new client.Object
	}{
		{"Agent provider ref", providerRefsChanged(),
			watchAgent("c", []string{"mp"}, nil, ""), watchAgent("c", []string{"mp", "mp2"}, nil, "")},
		{"AgentTask provider ref", providerRefsChanged(),
			watchTask("c", []string{"mp"}, nil), watchTask("c", []string{"mp2"}, nil)},
		{"Agent tool grant", toolGrantsChanged(),
			watchAgent("c", nil, []string{"tp"}, ""), watchAgent("c", nil, nil, "")},
		{"AgentTask tool grant", toolGrantsChanged(),
			watchTask("c", nil, []string{"tp"}), watchTask("c", nil, []string{"tp2"})},
		{"Agent agentClassRef", classUsageChanged(),
			watchAgent("c", nil, nil, ""), watchAgent("c2", nil, nil, "")},
		{"AgentTask agentClassRef", classUsageChanged(),
			watchTask("c", nil, nil), watchTask("c2", nil, nil)},
	}
	for _, tc := range cases {
		if !admitsUpdate(tc.pred, tc.old, tc.new) {
			t.Errorf("%s change was not admitted", tc.name)
		}
	}
}

// The class counts Agents in Replacing and ReplacementPending, so an Agent
// entering or leaving either state reaches it; other PodUpToDate moves do
// not.
func TestReferrerWatch_ClassUsageFollowsDriftStates(t *testing.T) {
	const other = "SpecDrift"
	cases := []struct {
		from, to string
		want     bool
	}{
		{"", kaalmv1beta1.ReasonReplacing, true},
		{kaalmv1beta1.ReasonReplacing, kaalmv1beta1.ReasonReplacementPending, true},
		{kaalmv1beta1.ReasonReplacementPending, "", true},
		{kaalmv1beta1.ReasonReplacing, other, true},
		{"", other, false},
		{other, "", false},
	}
	p := classUsageChanged()
	for _, tc := range cases {
		got := admitsUpdate(p, watchAgent("c", nil, nil, tc.from), watchAgent("c", nil, nil, tc.to))
		if got != tc.want {
			t.Errorf("PodUpToDate %q -> %q: admitted = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
	msgOnly := watchAgent("c", nil, nil, kaalmv1beta1.ReasonReplacing)
	changed := msgOnly.DeepCopy()
	changed.Status.Conditions[0].Message = "new message"
	if admitsUpdate(p, msgOnly, changed) {
		t.Error("a message-only PodUpToDate change was admitted")
	}
}

// createOrDelete admits only creates and deletes: a provider status write
// (a spend fold, a probe result) does not reach the AgentClass.
func TestReferrerWatch_CreateOrDelete(t *testing.T) {
	p := createOrDelete()
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "mp", ResourceVersion: "1"}}
	written := mp.DeepCopy()
	written.ResourceVersion = "2"
	written.Status.ObservedGeneration = 3
	if admitsUpdate(p, mp, written) {
		t.Error("createOrDelete admitted an update")
	}
	if !p.Create(event.CreateEvent{Object: mp}) || !p.Delete(event.DeleteEvent{Object: mp}) {
		t.Error("createOrDelete rejected a create or delete")
	}
	if p.Generic(event.GenericEvent{Object: mp}) {
		t.Error("createOrDelete admitted a generic event")
	}
}
