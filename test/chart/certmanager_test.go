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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

const releaseNamespace = "kaalm-system"

// bundle is the part of a trust-manager Bundle the tests read.
type bundle struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Sources []map[string]any `json:"sources"`
		Target  struct {
			ConfigMap         map[string]any `json:"configMap"`
			NamespaceSelector map[string]any `json:"namespaceSelector"`
		} `json:"target"`
	} `json:"spec"`
}

func helmTemplate(t *testing.T, template string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-n", releaseNamespace, "-s", "templates/" + template,
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

// renderBundles returns the chart's trust-manager Bundles keyed by name.
func renderBundles(t *testing.T, args ...string) map[string]bundle {
	t.Helper()
	out := helmTemplate(t, "certmanager.yaml", args...)
	bundles := map[string]bundle{}
	for _, doc := range strings.Split(out, "\n---\n") {
		var b bundle
		if err := yaml.Unmarshal([]byte(doc), &b); err != nil || b.Kind != "Bundle" {
			continue
		}
		bundles[b.Metadata.Name] = b
	}
	return bundles
}

// valuesFile writes a values file for one test and returns the -f args.
func valuesFile(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-f", p}
}

var kaalmCASource = map[string]any{"secret": map[string]any{"name": "kaalm-ca", "key": "tls.crt"}}

var systemSelector = map[string]any{
	"matchLabels": map[string]any{"kubernetes.io/metadata.name": releaseNamespace},
}

// The workload Bundle kaalm-ca follows bundleSelector; the operator Bundle
// kaalm-ca-system always targets the release namespace only.
func TestBundles_Defaults(t *testing.T) {
	bundles := renderBundles(t)
	if len(bundles) != 2 {
		t.Fatalf("rendered %d Bundles, want kaalm-ca and kaalm-ca-system", len(bundles))
	}
	for name, b := range bundles {
		if !reflect.DeepEqual(b.Spec.Sources, []map[string]any{kaalmCASource}) {
			t.Errorf("%s sources = %v, want only the kaalm-ca Secret", name, b.Spec.Sources)
		}
		if b.Spec.Target.ConfigMap["key"] != "ca.crt" {
			t.Errorf("%s target key = %v, want ca.crt", name, b.Spec.Target.ConfigMap["key"])
		}
	}
	if sel := bundles["kaalm-ca"].Spec.Target.NamespaceSelector; sel != nil {
		t.Errorf("kaalm-ca namespaceSelector = %v, want none by default", sel)
	}
	if sel := bundles["kaalm-ca-system"].Spec.Target.NamespaceSelector; !reflect.DeepEqual(sel, systemSelector) {
		t.Errorf("kaalm-ca-system namespaceSelector = %v, want %v", sel, systemSelector)
	}
}

// extraSources land in both Bundles after the kaalm-ca source, so the
// re-key runbook's second source survives helm upgrade and reaches the
// operator as well as the workloads.
func TestBundles_ExtraSourcesFollowKaalmCA(t *testing.T) {
	bundles := renderBundles(t, valuesFile(t, `
trustManager:
  extraSources:
    - secret:
        name: kaalm-ca-old
        key: tls.crt
`)...)
	want := []map[string]any{
		kaalmCASource,
		{"secret": map[string]any{"name": "kaalm-ca-old", "key": "tls.crt"}},
	}
	for _, name := range []string{"kaalm-ca", "kaalm-ca-system"} {
		if got := bundles[name].Spec.Sources; !reflect.DeepEqual(got, want) {
			t.Errorf("%s sources = %v, want %v", name, got, want)
		}
	}
}

// A narrow bundleSelector reaches only the workload Bundle; it can no longer
// cut the operator off from the CA.
func TestBundles_SelectorAppliesToWorkloadBundleOnly(t *testing.T) {
	bundles := renderBundles(t, valuesFile(t, `
trustManager:
  bundleSelector:
    matchLabels:
      kaalm.io/agents: "true"
`)...)
	wantWorkload := map[string]any{"matchLabels": map[string]any{"kaalm.io/agents": "true"}}
	if sel := bundles["kaalm-ca"].Spec.Target.NamespaceSelector; !reflect.DeepEqual(sel, wantWorkload) {
		t.Errorf("kaalm-ca namespaceSelector = %v, want %v", sel, wantWorkload)
	}
	if sel := bundles["kaalm-ca-system"].Spec.Target.NamespaceSelector; !reflect.DeepEqual(sel, systemSelector) {
		t.Errorf("kaalm-ca-system namespaceSelector = %v, want %v", sel, systemSelector)
	}
}

// caConfigMaps returns the ConfigMaps projected into the kaalm-tls volume of
// the Deployment a template renders.
func caConfigMaps(t *testing.T, template string, args ...string) []string {
	t.Helper()
	out := helmTemplate(t, template, args...)
	for _, doc := range strings.Split(out, "\n---\n") {
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || d.Kind != "Deployment" {
			continue
		}
		var names []string
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name != "kaalm-tls" || v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					names = append(names, s.ConfigMap.Name)
				}
			}
		}
		return names
	}
	t.Fatalf("%s rendered no Deployment\n%s", template, out)
	return nil
}

// The controller, gateway, and console read the CA from the operator
// Bundle's ConfigMap, never the selector-driven workload one.
func TestDeployments_MountOperatorCABundle(t *testing.T) {
	for _, tc := range []struct {
		template string
		args     []string
	}{
		{"controller.yaml", nil},
		{"gateway.yaml", nil},
		{"console.yaml", []string{"--set", "console.enabled=true"}},
	} {
		t.Run(tc.template, func(t *testing.T) {
			names := caConfigMaps(t, tc.template, tc.args...)
			if len(names) == 0 || names[0] != "kaalm-ca-system" {
				t.Fatalf("kaalm-tls ConfigMaps = %v, want kaalm-ca-system first", names)
			}
			for _, n := range names {
				if n == "kaalm-ca" {
					t.Errorf("kaalm-tls still projects the workload ConfigMap kaalm-ca: %v", names)
				}
			}
		})
	}
}
