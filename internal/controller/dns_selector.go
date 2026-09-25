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
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultDNSNamespaceLabels and DefaultDNSPodLabels select the cluster DNS
// Pods on kubeadm, EKS, GKE, AKS, k3s, and the upstream CoreDNS chart. They
// are the defaults of --dns-namespace-labels and --dns-pod-labels and of the
// chart value controller.networkPolicy.dnsSelector.
const (
	DefaultDNSNamespaceLabels = labelKeyNamespaceName + "=kube-system"
	DefaultDNSPodLabels       = "k8s-app=kube-dns"
)

// DNSSelector picks the peers of the DNS egress rule on every synthesized
// workload NetworkPolicy. A zero value selects the defaults.
type DNSSelector struct {
	NamespaceLabels map[string]string
	PodLabels       map[string]string
}

// ParseDNSSelector builds a DNSSelector from the two flag values, each a
// comma-separated list of key=value pairs. The namespace list must select
// something; an empty pod list allows every Pod in the selected namespaces.
func ParseDNSSelector(namespaceLabels, podLabels string) (DNSSelector, error) {
	ns, err := parseLabels(namespaceLabels)
	if err != nil {
		return DNSSelector{}, fmt.Errorf("--dns-namespace-labels: %w", err)
	}
	if len(ns) == 0 {
		return DNSSelector{}, fmt.Errorf("--dns-namespace-labels: at least one key=value pair is required")
	}
	pods, err := parseLabels(podLabels)
	if err != nil {
		return DNSSelector{}, fmt.Errorf("--dns-pod-labels: %w", err)
	}
	return DNSSelector{NamespaceLabels: ns, PodLabels: pods}, nil
}

func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not key=value", pair)
		}
		out[k] = v
	}
	return out, nil
}

// peer is the NetworkPolicy peer for the DNS egress rule.
func (d DNSSelector) peer() networkingv1.NetworkPolicyPeer {
	ns, pods := d.NamespaceLabels, d.PodLabels
	if len(ns) == 0 && len(pods) == 0 {
		ns, _ = parseLabels(DefaultDNSNamespaceLabels)
		pods, _ = parseLabels(DefaultDNSPodLabels)
	}
	peer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: ns},
	}
	if len(pods) > 0 {
		peer.PodSelector = &metav1.LabelSelector{MatchLabels: pods}
	}
	return peer
}
