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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestDesiredTaskPod_Shape(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-42", Namespace: "team-a"}}
	eff := effectiveTaskSpec{Image: "img:v1", HealthPort: 8080, PersistenceOn: true}
	pod := desiredTaskPod(task, eff, "agentry-system")

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("task pods must have restartPolicy Never")
	}
	c := pod.Spec.Containers[0]
	if c.LivenessProbe != nil || c.ReadinessProbe != nil {
		t.Error("task pods must carry no kubelet probes")
	}
	if pod.Spec.ServiceAccountName != "task-fix-42" {
		t.Errorf("SA name wrong: %q", pod.Spec.ServiceAccountName)
	}
	envMap := map[string]string{}
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AGENTRY_GATEWAY_ENDPOINT"] == "" || envMap["AGENTRY_TLS_CERT"] == "" {
		t.Errorf("contract env missing: %v", envMap)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "fix-42-workspace" {
			found = true
		}
	}
	if !found {
		t.Error("workspace volume missing")
	}
}

func TestDesiredTaskCertificate_Shape(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-42", Namespace: "team-a"}}
	cert := desiredTaskCertificate(task)
	if len(cert.Spec.DNSNames) != 1 || cert.Spec.DNSNames[0] != "fix-42.team-a.task.agentry.io" {
		t.Errorf("task SAN wrong: %v", cert.Spec.DNSNames)
	}
	if len(cert.Spec.Usages) != 1 || string(cert.Spec.Usages[0]) != "client auth" {
		t.Errorf("task cert must be client auth only: %v", cert.Spec.Usages)
	}
}

func TestDesiredCompletionRole_Scoping(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-42", Namespace: "team-a"}}
	role := desiredCompletionRole(task)
	rule := role.Rules[0]
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "fix-42-completion" {
		t.Errorf("role not name-scoped: %v", rule.ResourceNames)
	}
	for _, v := range rule.Verbs {
		if v == "create" || v == "get" || v == "list" || v == "delete" {
			t.Errorf("role must grant update/patch only, found %q", v)
		}
	}
	rb := desiredCompletionRoleBinding(task, "agentry-system")
	if rb.Subjects[0].Name != "agentry-gateway" || rb.Subjects[0].Namespace != "agentry-system" {
		t.Errorf("binding subject wrong: %+v", rb.Subjects[0])
	}
}

func TestDesiredTaskNetworkPolicy_NoIngress(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-42", Namespace: "team-a"}}
	class := &agentryv1alpha1.AgentClass{}
	np := desiredTaskNetworkPolicy(task, class, "agentry-system")
	if np.Spec.Ingress == nil || len(np.Spec.Ingress) != 0 {
		t.Errorf("task policy must declare an explicit empty ingress list, got %v", np.Spec.Ingress)
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Error("task policy must declare both policy types")
	}
	if len(np.Spec.Egress) < 2 {
		t.Error("gateway and DNS egress rules missing")
	}
}

func TestParseCompletionAndValidate(t *testing.T) {
	empty := parseCompletion(map[string]string{})
	if empty.Status != "" {
		t.Error("empty mailbox must parse to empty status")
	}

	p := parseCompletion(map[string]string{
		"status":          "success",
		"message":         "done",
		"artifact.pr-url": "https://example.com/pr/1",
	})
	if p.Status != "success" || p.Message != "done" || p.Artifacts["pr-url"] == "" {
		t.Errorf("parse wrong: %+v", p)
	}

	declared := []agentryv1alpha1.AgentTaskArtifact{{Name: "pr-url"}, {Name: "summary"}}
	// success requires every declared artifact.
	if msg := validateArtifactNames(p, declared); msg == "" {
		t.Error("missing declared artifact must fail on success")
	}
	// failure tolerates missing declared artifacts.
	pf := p
	pf.Status = "failure"
	if msg := validateArtifactNames(pf, declared); msg != "" {
		t.Errorf("failure with subset must pass: %s", msg)
	}
	// undeclared names fail in both branches.
	pu := parseCompletion(map[string]string{"status": "failure", "artifact.rogue": "x"})
	if msg := validateArtifactNames(pu, declared); msg == "" {
		t.Error("undeclared artifact must fail")
	}
}

func TestDeriveEffectiveTaskSpec_ClassDefaults(t *testing.T) {
	sc := testStorageClass
	rc := "gvisor"
	class := &agentryv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: agentryv1alpha1.AgentClassSpec{
			Image: agentryv1alpha1.AgentClassImage{DefaultImage: "reg/default:v1", PullPolicy: corev1.PullAlways},
			Resources: agentryv1alpha1.AgentClassResources{
				Defaults: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			},
			Persistence: agentryv1alpha1.AgentClassPersistence{
				DefaultSizeGi: 5, MaxSizeGi: 10, StorageClassName: &sc,
			},
			Runtime: agentryv1alpha1.AgentClassRuntime{RuntimeClassName: &rc},
		},
	}
	// Task with no image and no resources inherits class defaults; size clamps.
	size := int32(50)
	task := &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
		Spec: agentryv1alpha1.AgentTaskSpec{
			Persistence: agentryv1alpha1.AgentTaskPersistence{Enabled: true, SizeGi: &size},
			Providers: []agentryv1alpha1.AgentProviderReference{
				{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "p1"}},
			},
		},
	}
	eff := deriveEffectiveTaskSpec(task, class)
	if eff.Image != "reg/default:v1" {
		t.Errorf("class default image not applied: %q", eff.Image)
	}
	if cpu := eff.Resources.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("100m")) != 0 {
		t.Error("class default resources not applied")
	}
	if eff.PVCSizeGi != 10 {
		t.Errorf("size not clamped to class max: %d", eff.PVCSizeGi)
	}
	if eff.RuntimeClassName == nil || *eff.RuntimeClassName != "gvisor" {
		t.Error("runtimeClassName not propagated")
	}
	if eff.PullPolicy != corev1.PullAlways {
		t.Errorf("pull policy not propagated: %v", eff.PullPolicy)
	}
	if len(eff.Providers) != 1 || eff.Providers[0] != "p1" {
		t.Errorf("providers not derived: %v", eff.Providers)
	}

	// Task overrides win; a default size applies when unset.
	task.Spec.Image = "reg/custom:v2"
	task.Spec.Persistence.SizeGi = nil
	eff = deriveEffectiveTaskSpec(task, class)
	if eff.Image != "reg/custom:v2" {
		t.Errorf("task image override lost: %q", eff.Image)
	}
	if eff.PVCSizeGi != 5 {
		t.Errorf("class default size not applied: %d", eff.PVCSizeGi)
	}
}

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

func TestDesiredTaskPVC(t *testing.T) {
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix-1", Namespace: "team-a"}}
	sc := testStorageClass
	class := &agentryv1alpha1.AgentClass{
		Spec: agentryv1alpha1.AgentClassSpec{
			Persistence: agentryv1alpha1.AgentClassPersistence{StorageClassName: &sc},
		},
	}
	// Zero size defaults to 1Gi.
	pvc := desiredTaskPVC(task, class, effectiveTaskSpec{PVCSizeGi: 0})
	if pvc.Name != "fix-1-workspace" || pvc.Namespace != "team-a" {
		t.Errorf("naming wrong: %s/%s", pvc.Namespace, pvc.Name)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("access mode wrong: %v", pvc.Spec.AccessModes)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != testStorageClass {
		t.Errorf("storage class not propagated: %v", pvc.Spec.StorageClassName)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("default size not 1Gi: %s", q.String())
	}
	// A set size is honored.
	pvc = desiredTaskPVC(task, class, effectiveTaskSpec{PVCSizeGi: 5})
	q = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("size not honored: %s", q.String())
	}
}
