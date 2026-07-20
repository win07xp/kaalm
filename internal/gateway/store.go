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

package gateway

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// Store is the gateway's read surface over cluster state. The production
// implementation wraps a controller-runtime informer cache; tests use a
// map-backed fake.
type Store interface {
	AgentByName(ctx context.Context, namespace, name string) (*agentryv1alpha1.Agent, bool)
	TaskByName(ctx context.Context, namespace, name string) (*agentryv1alpha1.AgentTask, bool)
	ClassByName(ctx context.Context, name string) (*agentryv1alpha1.AgentClass, bool)
	ProviderByName(ctx context.Context, name string) (*agentryv1alpha1.ModelProvider, bool)
	// Credential resolves the provider's credential Secret key value.
	Credential(ctx context.Context, provider *agentryv1alpha1.ModelProvider) (string, error)
	// PodByIP resolves a source IP to a Pod for the cross-check and the
	// Mode 2 ownership precheck. ok is false when no Pod matches.
	PodByIP(ctx context.Context, ip string) (*corev1.Pod, bool)
	// ChannelByPath resolves a webhook path to its AgentChannel. Only
	// channels with Ready=True are returned: Ready gates routing admission.
	ChannelByPath(ctx context.Context, path string) (*agentryv1alpha1.AgentChannel, bool)
	// SecretValue reads one key of a Secret in a user namespace (the
	// per-channel scoped Role is what grants this in production).
	SecretValue(ctx context.Context, namespace, name, key string) (string, error)
}

// isAgentryManagedPod reports whether the Pod belongs to an Agent or AgentTask
// (ownerRef to either kind, or the Agentry-managed label set). Such Pods must
// use mTLS; the Mode 2 precheck rejects their bearer tokens.
func isAgentryManagedPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.APIVersion == agentryv1alpha1.GroupVersion.String() &&
			(ref.Kind == "Agent" || ref.Kind == "AgentTask") {
			return true
		}
	}
	if _, ok := pod.Labels["agentry.io/workload"]; ok {
		return true
	}
	return false
}
