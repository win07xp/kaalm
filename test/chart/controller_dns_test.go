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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// renderControllerDNSArgs runs helm template for the controller Deployment
// and returns the manager container's args.
func renderControllerDNSArgs(t *testing.T, args ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/controller.yaml",
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || d.Kind != "Deployment" {
			continue
		}
		return d.Spec.Template.Spec.Containers[0].Args
	}
	t.Fatalf("rendered output has no Deployment\n%s", out)
	return nil
}

// controller.networkPolicy.dnsSelector reaches the controller as the two
// --dns-*-labels flags (#197).
func TestControllerArgs_DNSSelector(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"default", nil, []string{
			"--dns-namespace-labels=kubernetes.io/metadata.name=kube-system",
			"--dns-pod-labels=k8s-app=kube-dns",
		}},
		// Helm merges maps with the defaults, so a different label key nulls
		// the default key, as the tuning guide shows.
		{"custom", []string{
			"--set-json", `controller.networkPolicy.dnsSelector={` +
				`"namespaceLabels":{"kubernetes.io/metadata.name":null,"dns":"true"},` +
				`"podLabels":{"k8s-app":null,"app":"coredns"}}`,
		}, []string{"--dns-namespace-labels=dns=true", "--dns-pod-labels=app=coredns"}},
		{"namespace only", []string{
			"--set", "controller.networkPolicy.dnsSelector.podLabels=null",
		}, []string{"--dns-pod-labels="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(renderControllerDNSArgs(t, tc.args...), "\n")
			for _, w := range tc.want {
				if !strings.Contains(got+"\n", w+"\n") {
					t.Errorf("args lack %q:\n%s", w, got)
				}
			}
		})
	}
}
