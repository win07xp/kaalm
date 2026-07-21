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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestProvidersForWorkload(t *testing.T) {
	ctx := context.Background()

	ag := &agentryv1alpha1.Agent{Spec: agentryv1alpha1.AgentSpec{
		Providers: []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "p1"}},
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "p2"}},
		},
	}}
	if reqs := providersForWorkload(ctx, ag); len(reqs) != 2 ||
		reqs[0].Name != "p1" || reqs[1].Name != "p2" {
		t.Errorf("agent providers not mapped: %v", reqs)
	}

	task := &agentryv1alpha1.AgentTask{Spec: agentryv1alpha1.AgentTaskSpec{
		Providers: []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "tp"}},
		},
	}}
	if reqs := providersForWorkload(ctx, task); len(reqs) != 1 || reqs[0].Name != "tp" {
		t.Errorf("task providers not mapped: %v", reqs)
	}

	// A non-workload object yields nil.
	if reqs := providersForWorkload(ctx, &corev1.Secret{}); reqs != nil {
		t.Errorf("non-workload object must map to nil: %v", reqs)
	}
}

func TestProvidersForClass(t *testing.T) {
	ctx := context.Background()
	ac := &agentryv1alpha1.AgentClass{Spec: agentryv1alpha1.AgentClassSpec{
		AllowedProviders: []agentryv1alpha1.LocalObjectReference{{Name: "a"}, {Name: "b"}},
	}}
	if reqs := providersForClass(ctx, ac); len(reqs) != 2 || reqs[0].Name != "a" || reqs[1].Name != "b" {
		t.Errorf("class allowedProviders not mapped: %v", reqs)
	}
	// A wrong type yields nil.
	if reqs := providersForClass(ctx, &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x"}}); reqs != nil {
		t.Errorf("non-AgentClass object must map to nil: %v", reqs)
	}
}
