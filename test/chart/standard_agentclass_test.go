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

// Package chart renders the Helm chart offline (helm template, no cluster) and
// asserts what the shipped default objects grant. This is the chart half of
// docs/src/operations/deployment.md, Sample resources.
package chart

import (
	"os/exec"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// renderStandardClass runs helm template for the standard AgentClass alone and
// decodes it. Extra args are appended, so a test can set values.
func renderStandardClass(t *testing.T, args ...string) kaalmv1beta1.AgentClass {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/standard-agentclass.yaml",
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var ac kaalmv1beta1.AgentClass
	if err := yaml.UnmarshalStrict(out, &ac); err != nil {
		t.Fatalf("decode rendered class: %v\n%s", err, out)
	}
	if ac.Name != "standard" {
		t.Fatalf("rendered class is %q, want standard\n%s", ac.Name, out)
	}
	return ac
}

// The shipped class sets hibernation timings, so it must also permit
// hibernation and the persistence hibernation requires (rules 24, 26, 29).
func TestStandardAgentClass_AllowsHibernationAndPersistence(t *testing.T) {
	ac := renderStandardClass(t)

	if !ac.Spec.Lifecycle.HibernationAllowed {
		t.Error("lifecycle.hibernationAllowed is false, so the class's hibernation timings are never read")
	}
	if !ac.Spec.Persistence.Enabled {
		t.Error("persistence.enabled is false, so hibernation cannot be used on this class (rule 29)")
	}
	if ac.Spec.Persistence.DefaultSizeGi <= 0 {
		t.Error("persistence.defaultSizeGi is unset, so a workload that omits sizeGi gets the controller's 1Gi floor")
	}
	if ac.Spec.Persistence.MaxSizeGi < ac.Spec.Persistence.DefaultSizeGi {
		t.Errorf("persistence.maxSizeGi %d is below defaultSizeGi %d, so every default is clamped",
			ac.Spec.Persistence.MaxSizeGi, ac.Spec.Persistence.DefaultSizeGi)
	}
}

// allowedProviders is an install-time value: empty by default, because the
// chart installs no ModelProvider and a class naming one that does not exist is
// Ready=False with InvalidReference.
func TestStandardAgentClass_AllowedProvidersComeFromValues(t *testing.T) {
	if got := renderStandardClass(t).Spec.AllowedProviders; len(got) != 0 {
		t.Errorf("default render lists allowedProviders %v, want none", got)
	}

	ac := renderStandardClass(t,
		"--set", "standardAgentClass.allowedProviders={anthropic-shared,openai-fallback}")
	want := []string{"anthropic-shared", "openai-fallback"}
	if len(ac.Spec.AllowedProviders) != len(want) {
		t.Fatalf("allowedProviders %v, want %v", ac.Spec.AllowedProviders, want)
	}
	for i, name := range want {
		if ac.Spec.AllowedProviders[i].Name != name {
			t.Errorf("allowedProviders[%d] is %q, want %q", i, ac.Spec.AllowedProviders[i].Name, name)
		}
	}
}

// The standard class writes the restricted baseline out, so the rendered
// object shows what runs (#194).
func TestStandardAgentClass_DeclaresRestrictedBaseline(t *testing.T) {
	ac := renderStandardClass(t)
	ps, cs := ac.Spec.Security.PodSecurityContext, ac.Spec.Security.ContainerSecurityContext
	if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || ps.SeccompProfile == nil {
		t.Errorf("podSecurityContext is not the restricted baseline: %+v", ps)
	}
	if cs == nil || cs.ReadOnlyRootFilesystem == nil || !*cs.ReadOnlyRootFilesystem ||
		cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation || cs.Capabilities == nil {
		t.Errorf("containerSecurityContext is not the restricted baseline: %+v", cs)
	}
}
