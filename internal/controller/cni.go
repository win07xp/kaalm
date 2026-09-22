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
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
)

// fqdnCapableGroups are the API groups whose presence signals a CNI that can
// enforce FQDN egress policies (AgentClass.spec.network.egress.allowedHosts).
// Cilium's CiliumNetworkPolicy supports toFQDNs; Calico Enterprise uses its own
// projectcalico.org API. Standard Kubernetes NetworkPolicy cannot express FQDN
// rules (docs/src/resources/agentclass.md, rule 20).
var fqdnCapableGroups = []string{
	"cilium.io",
	"crd.projectcalico.org",
}

// ProbeFQDNPolicySupport reports whether the cluster's CNI can enforce FQDN
// egress policies. It is a one-time discovery check; FQDNProbe caches the
// result for the process lifetime (docs/src/controller/reconcilers.md,
// AgentClassReconciler).
func ProbeFQDNPolicySupport(dc discovery.DiscoveryInterface) (bool, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		// A partial discovery error still yields the group list; only a hard
		// failure is fatal.
		if groups == nil || errors.IsServiceUnavailable(err) {
			return false, err
		}
	}
	present := map[string]bool{}
	for _, g := range groups.Groups {
		present[g.Name] = true
	}
	for _, g := range fqdnCapableGroups {
		if present[g] {
			return true, nil
		}
	}
	return false, nil
}

// FQDNProbe caches ProbeFQDNPolicySupport for the process lifetime, so the
// AgentClass, Agent, and AgentTask reconcilers share one answer. A failed
// probe is not cached: the next call probes again. Safe for concurrent use.
type FQDNProbe struct {
	dc discovery.DiscoveryInterface

	mu        sync.Mutex
	probed    bool
	supported bool
}

// NewFQDNProbe returns a probe that asks dc on first use.
func NewFQDNProbe(dc discovery.DiscoveryInterface) *FQDNProbe {
	return &FQDNProbe{dc: dc}
}

// Supported reports whether the CNI can enforce FQDN egress policies.
func (p *FQDNProbe) Supported() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.probed {
		return p.supported, nil
	}
	supported, err := ProbeFQDNPolicySupport(p.dc)
	if err != nil {
		return false, err
	}
	p.supported = supported
	p.probed = true
	return supported, nil
}
