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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// PodIPIndex is the cache index mapping status.podIP to Pods. Registered by
// cmd/gateway before the cache starts.
const PodIPIndex = "status.podIP"

// KubeStore is the production Store over a controller-runtime informer cache.
type KubeStore struct {
	// Reader is the cache-backed client.
	Reader client.Reader
	// OperatorNamespace hosts the provider credential Secrets.
	OperatorNamespace string
}

// AgentByName looks up an Agent in the cache.
func (k *KubeStore) AgentByName(ctx context.Context, ns, name string) (*kaalmv1alpha1.Agent, bool) {
	var a kaalmv1alpha1.Agent
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		return nil, false
	}
	return &a, true
}

// TaskByName looks up an AgentTask in the cache.
func (k *KubeStore) TaskByName(ctx context.Context, ns, name string) (*kaalmv1alpha1.AgentTask, bool) {
	var t kaalmv1alpha1.AgentTask
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		return nil, false
	}
	return &t, true
}

// ClassByName looks up an AgentClass in the cache.
func (k *KubeStore) ClassByName(ctx context.Context, name string) (*kaalmv1alpha1.AgentClass, bool) {
	var c kaalmv1alpha1.AgentClass
	if err := k.Reader.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
		return nil, false
	}
	return &c, true
}

// ProviderByName looks up a ModelProvider in the cache.
func (k *KubeStore) ProviderByName(ctx context.Context, name string) (*kaalmv1alpha1.ModelProvider, bool) {
	var p kaalmv1alpha1.ModelProvider
	if err := k.Reader.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		return nil, false
	}
	return &p, true
}

// Credential reads the provider's credential Secret key from the operator
// namespace via the cache (which doubles as the rotation watch: an updated
// Secret is re-read on the next request).
func (k *KubeStore) Credential(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) (string, error) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: k.OperatorNamespace, Name: provider.Spec.CredentialsRef.Name}
	if err := k.Reader.Get(ctx, key, &sec); err != nil {
		return "", err
	}
	val, ok := sec.Data[provider.Spec.CredentialsRef.Key]
	if !ok || len(val) == 0 {
		return "", fmt.Errorf("key %q missing in Secret %s", provider.Spec.CredentialsRef.Key, key)
	}
	return string(val), nil
}

// ChannelByPath scans AgentChannels for a Ready channel registered at path.
// The prefix defense (the path must begin with the channel's own
// /channels/{namespace}/ prefix) is enforced here, independent of the
// reconciler's InvalidPath status.
func (k *KubeStore) ChannelByPath(ctx context.Context, path string) (*kaalmv1alpha1.AgentChannel, bool) {
	var channels kaalmv1alpha1.AgentChannelList
	if err := k.Reader.List(ctx, &channels); err != nil {
		return nil, false
	}
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.Webhook.Path != path {
			continue
		}
		if !channelPathAllowed(ch) {
			continue
		}
		for _, c := range ch.Status.Conditions {
			if c.Type == kaalmv1alpha1.ConditionReady && c.Status == "True" {
				return ch, true
			}
		}
	}
	return nil, false
}

// channelPathAllowed is the gateway-side half of validation rule 15.
func channelPathAllowed(ch *kaalmv1alpha1.AgentChannel) bool {
	return strings.HasPrefix(ch.Spec.Webhook.Path, "/channels/"+ch.Namespace+"/")
}

// SecretValue reads one Secret key from a user namespace.
func (k *KubeStore) SecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	var sec corev1.Secret
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sec); err != nil {
		return "", err
	}
	val, ok := sec.Data[key]
	if !ok || len(val) == 0 {
		return "", fmt.Errorf("key %q missing in Secret %s/%s", key, namespace, name)
	}
	return string(val), nil
}

// PodByIP resolves a source IP via the status.podIP cache index.
func (k *KubeStore) PodByIP(ctx context.Context, ip string) (*corev1.Pod, bool) {
	var pods corev1.PodList
	if err := k.Reader.List(ctx, &pods, client.MatchingFields{PodIPIndex: ip}); err != nil {
		return nil, false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		// Skip terminated Pods whose IP may already be recycled.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		return p, true
	}
	return nil, false
}
