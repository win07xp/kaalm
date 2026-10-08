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
	"sigs.k8s.io/yaml"
)

// renderGatewayArgs runs helm template for the gateway manifests, decodes the
// gateway Deployment, and returns its container args. Extra args are appended,
// so a test can set values.
func renderGatewayArgs(t *testing.T, args ...string) []string {
	t.Helper()
	d := renderGatewayDeployment(t, args...)
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "gateway" {
			return c.Args
		}
	}
	t.Fatalf("the gateway Deployment has no gateway container")
	return nil
}

// renderGatewayDeployment runs helm template for the gateway manifests and
// returns the gateway Deployment. Extra args are appended, so a test can set
// values.
func renderGatewayDeployment(t *testing.T, args ...string) appsv1.Deployment {
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
		return d
	}
	t.Fatalf("rendered output has no gateway Deployment\n%s", out)
	return appsv1.Deployment{}
}

// gracePeriod returns a Deployment's terminationGracePeriodSeconds, or -1
// when it is unset.
func gracePeriod(d appsv1.Deployment) int64 {
	if p := d.Spec.Template.Spec.TerminationGracePeriodSeconds; p != nil {
		return *p
	}
	return -1
}

// gateway.shutdown reaches the drain flags, and the Pod's grace period is
// drainDelay + timeout + 5s, so Kubernetes never kills a draining gateway.
func TestGateway_ShutdownValuesReachFlagsAndGracePeriod(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantArgs  []string
		wantGrace int64
	}{
		{"default", nil, []string{"--drain-delay=5s", "--shutdown-timeout=30s"}, 40},
		{"custom", []string{"--set", "gateway.shutdown.drainDelay=10s", "--set", "gateway.shutdown.timeout=2m"},
			[]string{"--drain-delay=10s", "--shutdown-timeout=2m"}, 135},
		{"no delay", []string{"--set", "gateway.shutdown.drainDelay=0s"},
			[]string{"--drain-delay=0s", "--shutdown-timeout=30s"}, 35},
		{"hours and minutes", []string{"--set", "gateway.shutdown.timeout=1h1m30s"},
			[]string{"--shutdown-timeout=1h1m30s"}, 5 + 3690 + 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := renderGatewayDeployment(t, tc.args...)
			args := d.Spec.Template.Spec.Containers[0].Args
			for _, want := range tc.wantArgs {
				if !slices.Contains(args, want) {
					t.Errorf("gateway args %v lack %s", args, want)
				}
			}
			if got := gracePeriod(d); got != tc.wantGrace {
				t.Errorf("terminationGracePeriodSeconds = %d, want %d", got, tc.wantGrace)
			}
		})
	}
}

// A shutdown duration that is not whole hours, minutes, or seconds fails
// the render and names the value, because the grace period is computed in
// whole seconds.
func TestGateway_ShutdownRejectsMalformedDurations(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"drainDelay", `500ms`},
		{"drainDelay", `5`},
		{"drainDelay", `"-5s"`},
		{"drainDelay", `abc`},
		{"timeout", `1.5s`},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			renderFails(t, "gateway:\n  shutdown:\n    "+tc.key+": "+tc.value+"\n",
				"gateway.shutdown."+tc.key+" must be a duration in whole hours, minutes, or seconds")
		})
	}
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

// gateway.mcpMaxBodyBytes and gateway.mcpUpstreamTimeout are the tool
// broker's own body cap and whole-call timeout, separate from the LLM proxy's.
func TestGateway_MCPLimitsReachGatewayFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"default", nil, []string{"--mcp-max-body-bytes=4194304", "--mcp-upstream-timeout=120s"}},
		{"custom", []string{
			"--set", "gateway.mcpMaxBodyBytes=8Mi",
			"--set", "gateway.mcpUpstreamTimeout=300s",
		}, []string{"--mcp-max-body-bytes=8388608", "--mcp-upstream-timeout=300s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := renderGatewayArgs(t, tc.args...)
			for _, want := range tc.want {
				found := false
				for _, a := range args {
					if a == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("gateway args %v do not contain %s", args, want)
				}
			}
		})
	}
}
