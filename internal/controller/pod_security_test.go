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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func hasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// A class with no security block yields the restricted baseline on both
// kinds of Pod, with an emptyDir at /tmp (#194).
func TestDesiredPods_RestrictedBaseline(t *testing.T) {
	class := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "plain"},
		Spec: kaalmv1beta1.AgentClassSpec{Image: kaalmv1beta1.AgentClassImage{DefaultImage: "img"}}}
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team-a"}}
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "team-a"}}
	pods := []*corev1.Pod{
		desiredPod(agent, deriveEffectiveSpec(agent, class), "kaalm-system"),
		desiredTaskPod(task, deriveEffectiveTaskSpec(task, class), "kaalm-system"),
	}
	for _, pod := range pods {
		ps, cs := pod.Spec.SecurityContext, pod.Spec.Containers[0].SecurityContext
		if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot ||
			ps.SeccompProfile == nil || ps.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s pod security context is not the restricted baseline: %+v", pod.Kind, ps)
		}
		if cs == nil || cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation ||
			cs.ReadOnlyRootFilesystem == nil || !*cs.ReadOnlyRootFilesystem ||
			cs.Capabilities == nil || len(cs.Capabilities.Drop) != 1 || cs.Capabilities.Drop[0] != "ALL" {
			t.Errorf("container security context is not the restricted baseline: %+v", cs)
		}
		if !hasVolume(pod, tmpVolumeName) {
			t.Errorf("read-only root without an emptyDir at /tmp: %v", pod.Spec.Volumes)
		}
	}
}

// A declared field wins over the baseline; an unset one takes it. A writable
// root gets no /tmp volume.
func TestDesiredPod_ClassSecurityOverridesPerField(t *testing.T) {
	uid := int64(10001)
	class := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "relaxed"},
		Spec: kaalmv1beta1.AgentClassSpec{
			Image: kaalmv1beta1.AgentClassImage{DefaultImage: "img"},
			Security: kaalmv1beta1.AgentClassSecurity{
				PodSecurityContext:       &corev1.PodSecurityContext{RunAsUser: &uid},
				ContainerSecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(false)},
			},
		}}
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team-a"}}
	pod := desiredPod(agent, deriveEffectiveSpec(agent, class), "kaalm-system")
	ps, cs := pod.Spec.SecurityContext, pod.Spec.Containers[0].SecurityContext
	if ps.RunAsUser == nil || *ps.RunAsUser != uid || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot {
		t.Errorf("declared runAsUser lost or baseline runAsNonRoot missing: %+v", ps)
	}
	if cs.ReadOnlyRootFilesystem == nil || *cs.ReadOnlyRootFilesystem || cs.AllowPrivilegeEscalation == nil {
		t.Errorf("declared readOnlyRootFilesystem=false lost or baseline missing: %+v", cs)
	}
	if hasVolume(pod, tmpVolumeName) {
		t.Error("writable root must not get the /tmp emptyDir")
	}
	if class.Spec.Security.ContainerSecurityContext.AllowPrivilegeEscalation != nil {
		t.Error("the class's declared context was mutated")
	}
}

func TestSecurityBaselineDeviations(t *testing.T) {
	if d := securityBaselineDeviations(kaalmv1beta1.AgentClassSecurity{}); len(d) != 0 {
		t.Errorf("unset block is not a deviation: %v", d)
	}
	sec := kaalmv1beta1.AgentClassSecurity{
		PodSecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: boolPtr(false)},
		ContainerSecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(true),
			Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE", "SYS_ADMIN"}},
		},
	}
	d := securityBaselineDeviations(sec)
	joined := strings.Join(d, "; ")
	for _, want := range []string{"runAsNonRoot is false", "allowPrivilegeEscalation is true", "add includes SYS_ADMIN"} {
		if !strings.Contains(joined, want) {
			t.Errorf("deviations %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "NET_BIND_SERVICE") || len(d) != 3 {
		t.Errorf("NET_BIND_SERVICE is allowed under restricted; got %v", d)
	}
}
