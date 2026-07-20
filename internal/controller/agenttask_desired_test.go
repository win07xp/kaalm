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
