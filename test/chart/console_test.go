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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// renderConsole runs helm template for the console file with the console
// enabled and returns its ClusterRole and Deployment.
func renderConsole(t *testing.T, args ...string) (rbacv1.ClusterRole, appsv1.Deployment) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/console.yaml", "--set", "console.enabled=true",
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var role rbacv1.ClusterRole
	var dep appsv1.Deployment
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var meta struct{ Kind string }
		if yaml.Unmarshal([]byte(doc), &meta) != nil {
			continue
		}
		switch meta.Kind {
		case "ClusterRole":
			if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
				t.Fatal(err)
			}
		case "Deployment":
			if err := yaml.Unmarshal([]byte(doc), &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	return role, dep
}

// The console ClusterRole grants only the kaalm.io kinds the data layer
// reads (#224): Agent, AgentTask, AgentChannel, and ModelProvider.
func TestConsole_ClusterRoleGrantsOnlyTheKindsItReads(t *testing.T) {
	role, _ := renderConsole(t)
	var kinds []string
	for _, r := range role.Rules {
		if slices.Contains(r.APIGroups, "kaalm.io") {
			kinds = append(kinds, r.Resources...)
			if !slices.Equal(r.Verbs, []string{"get", "list", "watch"}) {
				t.Errorf("kaalm.io verbs = %v, want read only", r.Verbs)
			}
		}
	}
	slices.Sort(kinds)
	want := []string{"agentchannels", "agents", "agenttasks", "modelproviders"}
	if !slices.Equal(kinds, want) {
		t.Errorf("console kaalm.io resources = %v, want %v", kinds, want)
	}
}

// The console caps its chat body at the gateway's own test-chat cap: both
// read gateway.maxMessageBodyBytes.
func TestConsole_ChatBodyCapFollowsTheGateway(t *testing.T) {
	for value, want := range map[string]string{"": "1048576", "2Mi": "2097152"} {
		var args []string
		if value != "" {
			args = []string{"--set", "gateway.maxMessageBodyBytes=" + value}
		}
		_, dep := renderConsole(t, args...)
		if len(dep.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("console containers = %d", len(dep.Spec.Template.Spec.Containers))
		}
		flag := "--max-message-body-bytes=" + want
		if !slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, flag) {
			t.Errorf("gateway.maxMessageBodyBytes=%q: console args %v lack %s",
				value, dep.Spec.Template.Spec.Containers[0].Args, flag)
		}
	}
}
