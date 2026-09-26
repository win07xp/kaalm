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

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// renderNetworkPolicies runs helm template for the operator's own
// NetworkPolicy objects and returns them by name.
func renderNetworkPolicies(t *testing.T, args ...string) map[string]networkingv1.NetworkPolicy {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"),
		"-s", "templates/networkpolicy.yaml",
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "could not find template") {
			return nil
		}
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	got := map[string]networkingv1.NetworkPolicy{}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &np); err != nil || np.Kind != "NetworkPolicy" {
			continue
		}
		got[np.Name] = np
	}
	return got
}

// ruleForPort returns the ingress rule that admits the port.
func ruleForPort(np networkingv1.NetworkPolicy, port int32) *networkingv1.NetworkPolicyIngressRule {
	for i := range np.Spec.Ingress {
		for _, p := range np.Spec.Ingress[i].Ports {
			if p.Port != nil && p.Port.IntVal == port {
				return &np.Spec.Ingress[i]
			}
		}
	}
	return nil
}

// The chart ships default-deny ingress for its own Pods, with the metrics
// ports open only to networkPolicy.metricsFrom (#218).
func TestNetworkPolicy_Defaults(t *testing.T) {
	nps := renderNetworkPolicies(t)
	if len(nps) != 2 {
		t.Fatalf("want controller and gateway policies, got %v", nps)
	}
	for name, port := range map[string]int32{"kaalm-controller": 8080, "kaalm-gateway": 9090} {
		np := nps[name]
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
			t.Errorf("%s policyTypes = %v, want Ingress only", name, np.Spec.PolicyTypes)
		}
		rule := ruleForPort(np, port)
		if rule == nil || len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil ||
			rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "monitoring" {
			t.Errorf("%s metrics port %d is not limited to the monitoring namespace: %+v", name, port, rule)
		}
	}
	activator := ruleForPort(nps["kaalm-controller"], 9443)
	if activator == nil || len(activator.From) != 1 || activator.From[0].PodSelector == nil ||
		activator.From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "gateway" {
		t.Errorf("activator port is not limited to the gateway: %+v", activator)
	}
	if r := ruleForPort(nps["kaalm-controller"], 9444); r == nil || len(r.From) != 0 {
		t.Errorf("conversion port must admit any source: %+v", r)
	}
	cluster := ruleForPort(nps["kaalm-gateway"], 8443)
	if cluster == nil || len(cluster.From) != 1 || cluster.From[0].NamespaceSelector == nil ||
		len(cluster.From[0].NamespaceSelector.MatchLabels) != 0 {
		t.Errorf("cluster listener must admit every namespace: %+v", cluster)
	}
	if r := ruleForPort(nps["kaalm-gateway"], 8080); r == nil || len(r.From) != 0 {
		t.Errorf("user listener must admit any source by default: %+v", r)
	}
}

func TestNetworkPolicy_Values(t *testing.T) {
	nps := renderNetworkPolicies(t,
		"--set", "console.enabled=true",
		"--set-json", `networkPolicy.metricsFrom=[{"podSelector":{"matchLabels":{"app":"prometheus"}}}]`,
		"--set-json", `networkPolicy.userListenerFrom=`+
			`[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ingress"}}}]`,
	)
	if len(nps) != 3 {
		t.Fatalf("want three policies with the console on, got %d", len(nps))
	}
	if r := ruleForPort(nps["kaalm-gateway"], 9090); r == nil || len(r.From) != 1 || r.From[0].PodSelector == nil ||
		r.From[0].PodSelector.MatchLabels["app"] != "prometheus" {
		t.Errorf("metricsFrom did not reach the gateway metrics rule: %+v", r)
	}
	for _, np := range []networkingv1.NetworkPolicy{nps["kaalm-gateway"], nps["kaalm-console"]} {
		port := int32(8080)
		if np.Name == "kaalm-console" {
			port = 8443
		}
		if r := ruleForPort(np, port); r == nil || len(r.From) != 1 || r.From[0].NamespaceSelector == nil ||
			r.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ingress" {
			t.Errorf("%s userListenerFrom did not reach port %d: %+v", np.Name, port, r)
		}
	}
	if nps := renderNetworkPolicies(t, "--set", "networkPolicy.enabled=false"); len(nps) != 0 {
		t.Errorf("networkPolicy.enabled=false still renders %d policies", len(nps))
	}
}
