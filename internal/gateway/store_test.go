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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestIsKaalmManagedPod(t *testing.T) {
	// OwnerRef to an Agent.
	agentPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kaalmv1alpha1.GroupVersion.String(), Kind: "Agent"}},
	}}
	if !isKaalmManagedPod(agentPod) {
		t.Error("Agent-owned pod must be managed")
	}
	// OwnerRef to an AgentTask.
	taskPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kaalmv1alpha1.GroupVersion.String(), Kind: "AgentTask"}},
	}}
	if !isKaalmManagedPod(taskPod) {
		t.Error("AgentTask-owned pod must be managed")
	}
	// Label-based.
	labeledPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kaalm.io/workload": "agent"}}}
	if !isKaalmManagedPod(labeledPod) {
		t.Error("labeled pod must be managed")
	}
	// Plain pod: not managed. An unrelated ownerRef must not match.
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet"}},
	}}
	if isKaalmManagedPod(plain) {
		t.Error("plain pod must not be managed")
	}
}
