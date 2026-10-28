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

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// TestAgentChangeMatters: the channel's Agent watch admits a change to each
// Agent field a channel pass reads (status.phase, the effective
// service.enabled), every create, delete, and generic event, and nothing
// else: a status write that keeps the phase, a label touch, and a spec edit
// the channel does not read wake no channel.
func TestAgentChangeMatters(t *testing.T) {
	p := agentChangeMatters()
	base := func() *kaalmv1beta1.Agent {
		return &kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", Generation: 1, ResourceVersion: "1"},
			Status:     kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning},
		}
	}
	withPhase := func(ph kaalmv1beta1.AgentPhase) *kaalmv1beta1.Agent {
		ag := base()
		ag.Status.Phase = ph
		return ag
	}
	withService := func(svc *kaalmv1beta1.AgentService) *kaalmv1beta1.Agent {
		ag := base()
		ag.Spec.Service = svc
		return ag
	}

	statusOnlyWrite := base()
	statusOnlyWrite.ResourceVersion = "2"
	statusOnlyWrite.Status.ObservedGeneration = 1
	apimeta.SetStatusCondition(&statusOnlyWrite.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Probe",
	})
	labelTouch := base()
	labelTouch.Labels = map[string]string{"touched": "yes"}
	imageEdit := base()
	imageEdit.Spec.Image = "example.com/agent:v2"
	imageEdit.Generation = 2

	cases := []struct {
		name     string
		old, new client.Object
		want     bool
	}{
		{"phase Provisioning to Running", withPhase(kaalmv1beta1.AgentProvisioning), withPhase(kaalmv1beta1.AgentRunning), true},
		{"phase Running to Degraded", withPhase(kaalmv1beta1.AgentRunning), withPhase(kaalmv1beta1.AgentDegraded), true},
		{"service nil to disabled", withService(nil), withService(&kaalmv1beta1.AgentService{Enabled: false}), true},
		{"service disabled to enabled", withService(&kaalmv1beta1.AgentService{Enabled: false}), withService(&kaalmv1beta1.AgentService{Enabled: true}), true},
		{"service nil to enabled", withService(nil), withService(&kaalmv1beta1.AgentService{Enabled: true}), false},
		{"status write with the same phase", base(), statusOnlyWrite, false},
		{"label touch", base(), labelTouch, false},
		{"spec.image edit", base(), imageEdit, false},
		{"not Agents", &kaalmv1beta1.AgentTask{}, &kaalmv1beta1.AgentTask{}, true},
	}
	for _, tc := range cases {
		if got := admitsUpdate(p, tc.old, tc.new); got != tc.want {
			t.Errorf("%s: admitted %v, want %v", tc.name, got, tc.want)
		}
	}

	if !p.Create(event.CreateEvent{Object: base()}) {
		t.Error("an Agent create must enqueue its channels")
	}
	if !p.Delete(event.DeleteEvent{Object: base()}) {
		t.Error("an Agent delete must enqueue its channels")
	}
	if !p.Generic(event.GenericEvent{Object: base()}) {
		t.Error("a generic Agent event must enqueue its channels")
	}
}
