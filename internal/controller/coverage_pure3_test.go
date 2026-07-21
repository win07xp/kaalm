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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestAgentReconciler_NowUsesClock(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := &AgentReconciler{Clock: func() time.Time { return fixed }}
	if got := r.now(); !got.Equal(fixed) {
		t.Errorf("now() = %v, want injected clock %v", got, fixed)
	}
	// A nil clock falls back to wall time (just prove it returns something recent).
	def := &AgentReconciler{}
	if def.now().IsZero() {
		t.Error("default now() must return wall time")
	}
}

func TestClassForWorkload(t *testing.T) {
	ctx := context.Background()

	// An Agent maps to its class.
	ag := &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec:       agentryv1alpha1.AgentSpec{AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "cls"}},
	}
	if reqs := classForWorkload(ctx, ag); len(reqs) != 1 || reqs[0].Name != "cls" {
		t.Errorf("agent should map to its class: %v", reqs)
	}

	// An AgentTask maps to its class.
	task := &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
		Spec:       agentryv1alpha1.AgentTaskSpec{AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "tcls"}},
	}
	if reqs := classForWorkload(ctx, task); len(reqs) != 1 || reqs[0].Name != "tcls" {
		t.Errorf("task should map to its class: %v", reqs)
	}

	// An empty class ref yields no requests.
	empty := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "e"}}
	if reqs := classForWorkload(ctx, empty); reqs != nil {
		t.Errorf("empty classRef must map to nil: %v", reqs)
	}

	// An unrelated object type yields no requests.
	if reqs := classForWorkload(ctx, &corev1.Secret{}); reqs != nil {
		t.Errorf("non-workload object must map to nil: %v", reqs)
	}
}
