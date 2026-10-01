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

// ciliumPolicyResource is the resource the probe looks for in
// ciliumPolicyGVK's group version. Only Cilium is supported: the controller
// writes a CiliumNetworkPolicy with toFQDNs. The cilium.io group alone is not
// enough, because Tetragon installs CRDs in cilium.io (TracingPolicy, in
// cilium.io/v1alpha1) on any CNI. Calico is not supported because
// crd.projectcalico.org exists on open-source Calico too, which has no
// domain-based egress, and the controller writes no Calico policy. Standard
// Kubernetes NetworkPolicy cannot express FQDN rules
// (docs/src/resources/agentclass.md, rule 20).
const ciliumPolicyResource = "ciliumnetworkpolicies"

// ProbeFQDNPolicySupport reports whether the cluster's CNI can enforce FQDN
// egress policies: the cluster serves the ciliumnetworkpolicies resource in
// cilium.io/v2. It is a one-time discovery check; FQDNProbe caches the result
// for the process lifetime (docs/src/controller/reconcilers.md,
// AgentClassReconciler).
func ProbeFQDNPolicySupport(dc discovery.DiscoveryInterface) (bool, error) {
	list, err := dc.ServerResourcesForGroupVersion(ciliumPolicyGVK.GroupVersion().String())
	if err != nil {
		// The API server answers NotFound for a group version it does not
		// serve; any other failure is no answer at all.
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Name == ciliumPolicyResource {
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
