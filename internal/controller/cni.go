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
// egress policies. It is a one-time discovery check; the result is cached for the
// process lifetime by the caller (docs/src/controller/reconcilers.md,
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
