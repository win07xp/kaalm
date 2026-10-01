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
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The sandboxed AgentClass sample is the class scenario S2 points at: gVisor
// Pods, a narrow image allowlist, bounded resources and storage, no extra
// egress, and the restricted baseline with no deprecated field.
func TestAgentClassSample_Sandboxed(t *testing.T) {
	raw, err := os.ReadFile("../../config/samples/kaalm_v1beta1_agentclass_sandboxed.yaml")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	var ac kaalmv1beta1.AgentClass
	if err := yaml.UnmarshalStrict(raw, &ac); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	if ac.APIVersion != "kaalm.io/v1beta1" || ac.Kind != "AgentClass" {
		t.Errorf("sample is %s %s, want kaalm.io/v1beta1 AgentClass", ac.APIVersion, ac.Kind)
	}
	if ac.Name != "sandboxed" {
		t.Errorf("sample name = %q, want sandboxed", ac.Name)
	}
	spec := &ac.Spec
	if rc := spec.Runtime.RuntimeClassName; rc == nil || *rc != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", rc)
	}
	if len(spec.Image.AllowedImages) == 0 {
		t.Error("allowedImages is empty")
	}
	if spec.Image.AllowHandlerMounts {
		t.Error("allowHandlerMounts is true")
	}
	if !spec.Persistence.Enabled || spec.Persistence.MaxSizeGi <= 0 {
		t.Errorf("persistence = %+v, want enabled with a maxSizeGi", spec.Persistence)
	}
	for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if _, ok := spec.Resources.MaxLimits[r]; !ok {
			t.Errorf("maxLimits has no %s", r)
		}
	}
	if d := securityBaselineDeviations(spec.Security); len(d) > 0 {
		t.Errorf("sample is below the restricted baseline: %v", d)
	}
	if d := deprecatedFields(spec); len(d) > 0 {
		t.Errorf("sample sets deprecated fields: %v", d)
	}
	if bad := append(invalidCIDRs(&ac), invalidHosts(&ac)...); len(bad) > 0 {
		t.Errorf("sample egress is invalid: %v", bad)
	}
	if e := spec.Network.Egress; len(e.AllowedCIDRs) > 0 || len(e.AllowedHosts) > 0 {
		t.Errorf("sample opens egress beyond the gateway: %+v", e)
	}
}
