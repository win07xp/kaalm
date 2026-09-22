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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// renderContainer runs helm template for one template file and returns the
// first container of the Deployment it renders. Extra args are appended, so a
// test can set values.
func renderContainer(t *testing.T, template string, args ...string) corev1.Container {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/" + template,
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		if !strings.Contains(doc, "kind: Deployment") {
			continue
		}
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatalf("decode rendered Deployment: %v\n%s", err, doc)
		}
		if len(d.Spec.Template.Spec.Containers) == 0 {
			t.Fatalf("Deployment %s has no containers", d.Name)
		}
		return d.Spec.Template.Spec.Containers[0]
	}
	t.Fatalf("%s rendered no Deployment\n%s", template, out)
	return corev1.Container{}
}

func wantQuantity(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	got, ok := list[name]
	if !ok {
		t.Errorf("%s is unset, want %s", name, want)
		return
	}
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s is %s, want %s", name, got.String(), want)
	}
}

func wantArgs(t *testing.T, c corev1.Container, want ...string) {
	t.Helper()
	for _, arg := range want {
		if !slices.Contains(c.Args, arg) {
			t.Errorf("%s args %v lack %s", c.Name, c.Args, arg)
		}
	}
}

// The controller and gateway ship with memory-bounded requests and no CPU
// limit, and a custom value replaces the default verbatim.
func TestDeployments_Resources(t *testing.T) {
	for _, tc := range []struct{ template, prefix string }{
		{"controller.yaml", "controller"},
		{"gateway.yaml", "gateway"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			r := renderContainer(t, tc.template).Resources
			wantQuantity(t, r.Requests, corev1.ResourceCPU, "100m")
			wantQuantity(t, r.Requests, corev1.ResourceMemory, "128Mi")
			wantQuantity(t, r.Limits, corev1.ResourceMemory, "512Mi")
			if _, ok := r.Limits[corev1.ResourceCPU]; ok {
				t.Error("a CPU limit is set; the chart ships none so the process is never throttled")
			}

			r = renderContainer(t, tc.template,
				"--set", tc.prefix+".resources.requests.cpu=250m",
				"--set", tc.prefix+".resources.limits.memory=1Gi").Resources
			wantQuantity(t, r.Requests, corev1.ResourceCPU, "250m")
			wantQuantity(t, r.Limits, corev1.ResourceMemory, "1Gi")
		})
	}
}

// Log level and API client rate limits reach each binary as flags.
func TestDeployments_LoggingAndClientFlags(t *testing.T) {
	t.Run("controller", func(t *testing.T) {
		wantArgs(t, renderContainer(t, "controller.yaml"),
			"--zap-encoder=json", "--zap-log-level=info", "--client-qps=20", "--client-burst=30")
		wantArgs(t, renderContainer(t, "controller.yaml",
			"--set", "controller.logLevel=debug",
			"--set", "controller.client.qps=50",
			"--set", "controller.client.burst=80"),
			"--zap-log-level=debug", "--client-qps=50", "--client-burst=80")
	})
	t.Run("gateway", func(t *testing.T) {
		wantArgs(t, renderContainer(t, "gateway.yaml"),
			"--log-level=info", "--client-qps=100", "--client-burst=200")
		wantArgs(t, renderContainer(t, "gateway.yaml",
			"--set", "gateway.logLevel=warn",
			"--set", "gateway.client.qps=300",
			"--set", "gateway.client.burst=600"),
			"--log-level=warn", "--client-qps=300", "--client-burst=600")
	})
	t.Run("console", func(t *testing.T) {
		wantArgs(t, renderContainer(t, "console.yaml", "--set", "console.enabled=true"),
			"--log-level=info")
		wantArgs(t, renderContainer(t, "console.yaml",
			"--set", "console.enabled=true", "--set", "console.logLevel=debug"),
			"--log-level=debug")
	})
}
