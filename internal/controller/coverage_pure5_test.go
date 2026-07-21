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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestDesiredTaskPod_MergesClassPodMetadata(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-9", Namespace: "team-a"}}
	eff := effectiveTaskSpec{
		Image:          "img:v1",
		HealthPort:     8080,
		PodLabels:      map[string]string{"team": "payments", "tier": "batch"},
		PodAnnotations: map[string]string{"prometheus.io/scrape": "true"},
	}
	pod := desiredTaskPod(task, eff, "agentry-system")

	// Class-level pod labels merge with the task's identity labels.
	if pod.Labels["team"] != "payments" || pod.Labels["tier"] != "batch" {
		t.Errorf("class pod labels not merged: %v", pod.Labels)
	}
	if pod.Labels["agentry.io/task"] != "fix-9" {
		t.Errorf("task identity label missing: %v", pod.Labels)
	}
	if pod.Annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("class pod annotations not merged: %v", pod.Annotations)
	}
}

func TestDesiredPod_MergesClassPodMetadata(t *testing.T) {
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	eff := effectiveAgentSpec{
		Image:          "img:v1",
		HealthPort:     8080,
		PodLabels:      map[string]string{"team": "search"},
		PodAnnotations: map[string]string{"vault.io/inject": "false"},
	}
	pod := desiredPod(agent, eff, "agentry-system")
	if pod.Labels["team"] != "search" || pod.Labels["agentry.io/agent"] != "sup" {
		t.Errorf("class labels not merged with identity labels: %v", pod.Labels)
	}
	if pod.Annotations["vault.io/inject"] != "false" {
		t.Errorf("class annotations not merged: %v", pod.Annotations)
	}
	if pod.Annotations[annotationPodSpecHash] == "" {
		t.Error("pod-spec hash annotation must still be present")
	}
}

func TestDesiredTaskNetworkPolicy_AllowedCIDRs(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-9", Namespace: "team-a"}}
	class := &agentryv1alpha1.AgentClass{Spec: agentryv1alpha1.AgentClassSpec{
		Network: agentryv1alpha1.AgentClassNetwork{
			Egress: agentryv1alpha1.AgentClassEgress{AllowedCIDRs: []string{"203.0.113.0/24"}},
		},
	}}
	np := desiredTaskNetworkPolicy(task, class, "agentry-system")
	// gateway + DNS + one CIDR rule.
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("want 3 egress rules, got %d", len(np.Spec.Egress))
	}
	last := np.Spec.Egress[2]
	if last.To[0].IPBlock == nil || last.To[0].IPBlock.CIDR != "203.0.113.0/24" {
		t.Errorf("allowedCIDR egress rule missing: %+v", last)
	}
}
