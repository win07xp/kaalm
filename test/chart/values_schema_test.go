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
)

// schemaRejection starts every values-schema failure, in Helm 3 and Helm 4
// alike. The lines after it name the offending key in each version's own
// format, so tests match the key name only.
const schemaRejection = "values don't meet the specifications of the schema"

var chartDir = filepath.Join("..", "..", "charts", "kaalm")

// schemaFails renders the chart with args and wants the values schema to
// reject them, naming key.
func schemaFails(t *testing.T, key string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := runFullTemplate(args...)
	if err == nil {
		t.Fatalf("helm template succeeded, want a schema rejection naming %q\n%s", key, out)
	}
	for _, want := range []string{schemaRejection, key} {
		if !strings.Contains(string(out), want) {
			t.Errorf("helm template output lacks %q:\n%s", want, out)
		}
	}
}

// renders wants the chart to render with args.
func renders(t *testing.T, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	if out, err := runFullTemplate(args...); err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
}

// lintPasses runs helm lint on the chart's defaults and wants it to pass.
func lintPasses(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	if out, err := exec.Command("helm", "lint", chartDir).CombinedOutput(); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
}

// lintFails runs helm lint with a values file and wants it to fail with a
// schema rejection naming key.
func lintFails(t *testing.T, values, key string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := append([]string{"lint", chartDir}, valuesFile(t, values)...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil {
		t.Fatalf("helm lint succeeded, want a schema rejection naming %q\n%s", key, out)
	}
	for _, want := range []string{schemaRejection, key} {
		if !strings.Contains(string(out), want) {
			t.Errorf("helm lint output lacks %q:\n%s", want, out)
		}
	}
}

// A misspelled key fails instead of being ignored.
func TestValuesSchema_RejectsUnknownKey(t *testing.T) {
	schemaFails(t, "replica", "--set", "controller.replica=3")
}

// Every subject needs kind and name, and takes no other fields than
// rbac.authorization.k8s.io/v1 Subject has.
func TestValuesSchema_SubjectNeedsKindAndName(t *testing.T) {
	schemaFails(t, "platformAdmins", valuesFile(t, `
rbac:
  personas:
    enabled: true
    platformAdmins:
      - kind: Group
        apigroup: x
`)...)
}

// developers and namespaceSecretsAdmins map a namespace to a subject list,
// so a subject where the list belongs fails.
func TestValuesSchema_NamespaceMapsHoldSubjectLists(t *testing.T) {
	schemaFails(t, "developers", valuesFile(t, `
rbac:
  personas:
    enabled: true
    developers:
      team-a:
        kind: Group
        name: x
`)...)
}

// A byte size is a plain integer or a Ki, Mi, or Gi quantity; any other
// string would render as 0.
func TestValuesSchema_ByteSizes(t *testing.T) {
	schemaFails(t, "maxMessageBodyBytes", "--set", "gateway.maxMessageBodyBytes=1MB")
	renders(t, "--set", "gateway.maxMessageBodyBytes=1048576")
	renders(t, "--set", "gateway.maxMessageBodyBytes=900Ki")
}

// --set delivers a float as a string, so the float values accept a numeric
// string.
func TestValuesSchema_SetDeliversFloatsAsStrings(t *testing.T) {
	args := renderGatewayArgs(t,
		"--set", "gateway.tracing.otlpEndpoint=http://collector:4318",
		"--set", "gateway.tracing.sampleRatio=0.5",
		"--set", "gateway.client.qps=150.5",
	)
	for _, want := range []string{"--otlp-sample-ratio=0.5", "--client-qps=150.5"} {
		if !slices.Contains(args, want) {
			t.Errorf("gateway args lack %q: %v", want, args)
		}
	}
}

// A null deletes a default key, and a null namespace placeholder renders no
// binding; neither fails the schema.
func TestValuesSchema_NullValues(t *testing.T) {
	values := valuesFile(t, `
controller:
  networkPolicy:
    dnsSelector:
      podLabels:
        k8s-app: null
        app: coredns
rbac:
  personas:
    enabled: true
    developers:
      team-c: null
      team-a:
        - kind: Group
          name: x
`)
	r := renderRBAC(t, values...)
	if _, ok := r.roleBindings["team-a/kaalm-developer"]; !ok {
		t.Errorf("no kaalm-developer RoleBinding in team-a")
	}
	if _, ok := r.roleBindings["team-c/kaalm-developer"]; ok {
		t.Errorf("a null subject list rendered a kaalm-developer RoleBinding in team-c")
	}
	args := renderControllerDNSArgs(t, values...)
	if !slices.Contains(args, "--dns-pod-labels=app=coredns") {
		t.Errorf("controller args lack --dns-pod-labels=app=coredns: %v", args)
	}
}

// helm lint checks values against the schema: the chart's defaults pass, and
// a list where developers needs a map fails.
func TestValuesSchema_HelmLint(t *testing.T) {
	lintPasses(t)
	lintFails(t, `
rbac:
  personas:
    developers:
      - kind: Group
        name: team-a-devs
`, "developers")
}
