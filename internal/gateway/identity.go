/*
Copyright 2026.

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

// Package gateway implements the Kaalm Gateway: the LLM proxy listener on
// :8443 with per-path client authentication. See docs/src/gateways/.
package gateway

import (
	"crypto/x509"
	"fmt"
	"strings"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// WorkloadKind discriminates the two SAN shapes.
type WorkloadKind string

const (
	KindAgent     WorkloadKind = "Agent"
	KindAgentTask WorkloadKind = "AgentTask"
)

// Identity is the cryptographically attested (namespace, workload name,
// workload kind) triple extracted from a client certificate SAN.
type Identity struct {
	Namespace string
	Name      string
	Kind      WorkloadKind
}

// ErrNoRecognizedSAN and friends classify SAN extraction failures; all map to
// 403 invalid_cert on the wire.
var (
	errNoRecognizedSAN        = fmt.Errorf("no recognized workload SAN")
	errMultipleRecognizedSANs = fmt.Errorf("multiple recognized workload SANs")
	errBadLabelCount          = fmt.Errorf("recognized SAN has the wrong label count")
)

// ParseWorkloadSAN scans the certificate's DNS SAN list for exactly one entry
// in a recognized shape and enforces the exact label count (5 for
// {name}.{ns}.svc.cluster.local, 4 for {name}.{ns}.task.kaalm.io). Short-form
// Service SANs match no recognized suffix and are ignored. This label-count
// rule is defense in depth against dotted-name spoofing; see
// docs/src/gateways/llm/workload-identity.md.
func ParseWorkloadSAN(cert *x509.Certificate) (Identity, error) {
	var matched []Identity
	var badCount bool
	for _, san := range cert.DNSNames {
		labels := strings.Split(san, ".")
		switch {
		case strings.HasSuffix(san, "."+kaalmv1alpha1.AgentSANSuffix):
			if len(labels) != kaalmv1alpha1.AgentSANLabels {
				badCount = true
				continue
			}
			matched = append(matched, Identity{Namespace: labels[1], Name: labels[0], Kind: KindAgent})
		case strings.HasSuffix(san, "."+kaalmv1alpha1.TaskSANSuffix):
			if len(labels) != kaalmv1alpha1.TaskSANLabels {
				badCount = true
				continue
			}
			matched = append(matched, Identity{Namespace: labels[1], Name: labels[0], Kind: KindAgentTask})
		}
	}
	switch {
	case len(matched) == 1:
		return matched[0], nil
	case len(matched) > 1:
		return Identity{}, errMultipleRecognizedSANs
	case badCount:
		return Identity{}, errBadLabelCount
	default:
		return Identity{}, errNoRecognizedSAN
	}
}

// IsControllerCert reports whether the certificate's SAN list names the
// controller Service DNS, which authorizes the controller-only paths.
func IsControllerCert(cert *x509.Certificate, operatorNamespace string) bool {
	long := fmt.Sprintf("kaalm-controller.%s.svc.cluster.local", operatorNamespace)
	short := fmt.Sprintf("kaalm-controller.%s.svc", operatorNamespace)
	for _, san := range cert.DNSNames {
		if san == long || san == short {
			return true
		}
	}
	return false
}
