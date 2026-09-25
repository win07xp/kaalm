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

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// renderControllerArgs runs helm template for the controller Deployment and
// returns the manager container's args. Extra args are appended, so a test can
// set values.
func renderControllerArgs(t *testing.T, args ...string) []string {
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
	for _, doc := range bytes.Split(out, []byte("\n---\n")) {
		var dep appsv1.Deployment
		if err := yaml.Unmarshal(doc, &dep); err != nil || dep.Kind != "Deployment" {
			continue
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				return c.Args
			}
		}
	}
	t.Fatalf("no manager container in the rendered controller Deployment\n%s", out)
	return nil
}

// The per-workload Certificate lifetime defaults to the controller's own flag
// defaults (90d, renewed 30d before expiry).
func TestControllerArgs_CertLifetimeDefaults(t *testing.T) {
	args := renderControllerArgs(t)
	for _, want := range []string{"--cert-duration=2160h", "--cert-renew-before=720h"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
}

// controller.certificate.duration and renewBefore reach the controller flags.
func TestControllerArgs_CertLifetimeFromValues(t *testing.T) {
	args := renderControllerArgs(t,
		"--set", "controller.certificate.duration=24h",
		"--set", "controller.certificate.renewBefore=8h",
	)
	for _, want := range []string{"--cert-duration=24h", "--cert-renew-before=8h"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
}
