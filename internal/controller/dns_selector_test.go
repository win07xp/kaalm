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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestParseDNSSelector(t *testing.T) {
	sel, err := ParseDNSSelector(DefaultDNSNamespaceLabels, DefaultDNSPodLabels)
	if err != nil {
		t.Fatal(err)
	}
	if sel.NamespaceLabels[labelKeyNamespaceName] != "kube-system" || sel.PodLabels["k8s-app"] != "kube-dns" {
		t.Errorf("defaults parsed wrong: %+v", sel)
	}
	sel, err = ParseDNSSelector("a=1, b=2", "")
	if err != nil || len(sel.NamespaceLabels) != 2 || len(sel.PodLabels) != 0 {
		t.Errorf("two namespace labels and no pod labels: %+v, %v", sel, err)
	}
	for _, bad := range [][2]string{{"", "k8s-app=kube-dns"}, {"novalue", ""}, {"a=1", "=x"}} {
		if _, err := ParseDNSSelector(bad[0], bad[1]); err == nil {
			t.Errorf("ParseDNSSelector(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// The zero DNSSelector selects the defaults, and a custom selector reaches the
// DNS egress rule of both the Agent and the AgentTask policy (#197).
func TestDesiredNetworkPolicies_DNSSelector(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "team-a"}}
	class := &kaalmv1beta1.AgentClass{}
	custom := DNSSelector{NamespaceLabels: map[string]string{"dns": "true"}, PodLabels: map[string]string{"app": "coredns"}}
	nsOnly := DNSSelector{NamespaceLabels: map[string]string{"dns": "true"}}

	for _, tc := range []struct {
		name          string
		sel           DNSSelector
		wantNS        map[string]string
		wantPods      map[string]string
		wantPodsUnset bool
	}{
		{"zero selects the defaults", DNSSelector{},
			map[string]string{labelKeyNamespaceName: "kube-system"}, map[string]string{"k8s-app": "kube-dns"}, false},
		{"custom", custom, custom.NamespaceLabels, custom.PodLabels, false},
		{"namespace only", nsOnly, nsOnly.NamespaceLabels, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentDNS := desiredNetworkPolicy(agent, class, effectiveAgentSpec{HealthPort: 8080}, "kaalm-system", tc.sel).Spec.Egress[1].To[0]
			taskDNS := desiredTaskNetworkPolicy(task, class, "kaalm-system", tc.sel).Spec.Egress[1].To[0]
			for _, peer := range []struct {
				kind string
				ns   *metav1.LabelSelector
				pods *metav1.LabelSelector
			}{{"Agent", agentDNS.NamespaceSelector, agentDNS.PodSelector}, {"AgentTask", taskDNS.NamespaceSelector, taskDNS.PodSelector}} {
				if !equalLabels(peer.ns.MatchLabels, tc.wantNS) {
					t.Errorf("%s namespaceSelector = %v, want %v", peer.kind, peer.ns.MatchLabels, tc.wantNS)
				}
				if tc.wantPodsUnset {
					if peer.pods != nil {
						t.Errorf("%s podSelector = %v, want none", peer.kind, peer.pods.MatchLabels)
					}
				} else if peer.pods == nil || !equalLabels(peer.pods.MatchLabels, tc.wantPods) {
					t.Errorf("%s podSelector = %v, want %v", peer.kind, peer.pods, tc.wantPods)
				}
			}
		})
	}
}

func equalLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
