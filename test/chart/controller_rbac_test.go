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

package chart

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// renderControllerClusterRole runs helm template for the RBAC template and
// returns the controller ClusterRole.
func renderControllerClusterRole(t *testing.T) rbacv1.ClusterRole {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := exec.Command("helm", "template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/rbac.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, doc := range bytes.Split(out, []byte("\n---\n")) {
		var cr rbacv1.ClusterRole
		if err := yaml.Unmarshal(doc, &cr); err != nil {
			continue
		}
		if cr.Kind == "ClusterRole" && cr.Name == "kaalm-controller" {
			return cr
		}
	}
	t.Fatalf("no kaalm-controller ClusterRole in the render\n%s", out)
	return rbacv1.ClusterRole{}
}

// The controller writes the per-workload CiliumNetworkPolicy for allowedHosts
// (docs/src/security/rbac.md). The rule ships on every CNI: on a cluster
// without the cilium.io group it grants nothing.
func TestControllerClusterRole_GrantsCiliumNetworkPolicies(t *testing.T) {
	cr := renderControllerClusterRole(t)
	for _, r := range cr.Rules {
		if !slices.Contains(r.APIGroups, "cilium.io") || !slices.Contains(r.Resources, "ciliumnetworkpolicies") {
			continue
		}
		for _, verb := range []string{"get", "create", "update", "patch", "delete"} {
			if !slices.Contains(r.Verbs, verb) {
				t.Errorf("cilium.io ciliumnetworkpolicies rule lacks %q: %v", verb, r.Verbs)
			}
		}
		return
	}
	t.Error("the controller ClusterRole has no cilium.io ciliumnetworkpolicies rule")
}
