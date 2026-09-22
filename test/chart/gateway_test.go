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

// renderGatewayArgs runs helm template for the gateway manifests, decodes the
// gateway Deployment, and returns its container args. Extra args are appended,
// so a test can set values.
func renderGatewayArgs(t *testing.T, args ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/gateway.yaml",
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
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "gateway" {
				return c.Args
			}
		}
	}
	t.Fatalf("rendered output has no gateway Deployment container\n%s", out)
	return nil
}

// gateway.providerFirstByteTimeout is the gateway's --upstream-timeout, the
// per-attempt bound behind the fallback timeout trigger.
func TestGateway_ProviderFirstByteTimeoutReachesUpstreamTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, "--upstream-timeout=120s"},
		{"custom", []string{"--set", "gateway.providerFirstByteTimeout=45s"}, "--upstream-timeout=45s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := renderGatewayArgs(t, tc.args...)
			for _, a := range args {
				if a == tc.want {
					return
				}
			}
			t.Errorf("gateway args %v do not contain %s", args, tc.want)
		})
	}
}
