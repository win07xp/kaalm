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

package gateway

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/secretwatch"
)

// PodIPIndex is the cache index mapping status.podIP to Pods. Registered by
// cmd/gateway before the cache starts.
const PodIPIndex = "status.podIP"

// ChannelPathIndex is the cache index mapping an AgentChannel's path to the
// channels registered at it, across all namespaces. Registered by
// cmd/gateway before the cache starts, with ChannelPathIndexValue.
const ChannelPathIndex = "spec.path"

// ChannelPathIndexValue is the ChannelPathIndex extractor: the path of the
// block the channel's type selects. A channel with no path is not indexed.
func ChannelPathIndexValue(o client.Object) []string {
	if path := o.(*kaalmv1beta1.AgentChannel).Spec.Path(); path != "" {
		return []string{path}
	}
	return nil
}

// KubeStore is the production Store over a controller-runtime informer cache.
type KubeStore struct {
	// Reader is the cache-backed client.
	Reader client.Reader
	// APIReader is the uncached client backing PodByIPLive. Nil disables the
	// live fallback (informer-only resolution).
	APIReader client.Reader
	// OperatorNamespace hosts the provider credential Secrets.
	OperatorNamespace string
	// Secrets serves every Secret read from per-object watches. Nil reads
	// Secrets through Reader instead, which in production is the uncached
	// path: the cache is disabled for Secrets (cmd/gateway), so each read is
	// a live GET behind the client's rate limiter.
	Secrets *secretwatch.Watcher
}

// secret reads one Secret, through the watcher when there is one.
func (k *KubeStore) secret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	if k.Secrets != nil {
		return k.Secrets.Get(ctx, namespace, name)
	}
	var sec corev1.Secret
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}

// AgentByName looks up an Agent in the cache. The object is read-only: the
// gateway cache skips the deep copy for this kind (cmd/gateway), so its maps
// and slices are shared with the cache.
func (k *KubeStore) AgentByName(ctx context.Context, ns, name string) (*kaalmv1beta1.Agent, bool) {
	var a kaalmv1beta1.Agent
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		return nil, false
	}
	return &a, true
}

// TaskByName looks up an AgentTask in the cache. The object is read-only: the
// gateway cache skips the deep copy for this kind (cmd/gateway), so its maps
// and slices are shared with the cache.
func (k *KubeStore) TaskByName(ctx context.Context, ns, name string) (*kaalmv1beta1.AgentTask, bool) {
	var t kaalmv1beta1.AgentTask
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		return nil, false
	}
	return &t, true
}

// ClassByName looks up an AgentClass in the cache. The object is read-only: the
// gateway cache skips the deep copy for this kind (cmd/gateway), so its maps
// and slices are shared with the cache.
func (k *KubeStore) ClassByName(ctx context.Context, name string) (*kaalmv1beta1.AgentClass, bool) {
	var c kaalmv1beta1.AgentClass
	if err := k.Reader.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
		return nil, false
	}
	return &c, true
}

// ProviderByName looks up a ModelProvider in the cache. The object is read-only: the
// gateway cache skips the deep copy for this kind (cmd/gateway), so its maps
// and slices are shared with the cache.
func (k *KubeStore) ProviderByName(ctx context.Context, name string) (*kaalmv1beta1.ModelProvider, bool) {
	var p kaalmv1beta1.ModelProvider
	if err := k.Reader.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		return nil, false
	}
	return &p, true
}

// ToolProviderByName looks up a ToolProvider in the cache. The object is read-only: the
// gateway cache skips the deep copy for this kind (cmd/gateway), so its maps
// and slices are shared with the cache.
func (k *KubeStore) ToolProviderByName(ctx context.Context, name string) (*kaalmv1beta1.ToolProvider, bool) {
	var tp kaalmv1beta1.ToolProvider
	if err := k.Reader.Get(ctx, types.NamespacedName{Name: name}, &tp); err != nil {
		return nil, false
	}
	return &tp, true
}

// ToolCredential reads the tool provider's credential Secret key from the
// operator namespace, with the rule 49 and 50 checks, exactly as Credential
// does for ModelProvider. A nil credentialsRef is an unauthenticated server:
// no credential, no error.
func (k *KubeStore) ToolCredential(ctx context.Context, provider *kaalmv1beta1.ToolProvider) (string, error) {
	ref := provider.Spec.CredentialsRef
	if ref == nil {
		return "", nil
	}
	return k.providerCredential(ctx, provider.Spec.Endpoint, ref.Name, ref.Key)
}

// Credential reads the provider's credential Secret key from the operator
// namespace, with the rule 49 and 50 checks. With a secretwatch.Watcher the
// read is served from the Secret's own watch, so a rotation is visible on the
// next request without a GET.
func (k *KubeStore) Credential(ctx context.Context, provider *kaalmv1beta1.ModelProvider) (string, error) {
	ref := provider.Spec.CredentialsRef
	return k.providerCredential(ctx, provider.Spec.Endpoint, ref.Name, ref.Key)
}

// providerCredential reads one provider credential Secret from the operator
// namespace and returns its key only when the Secret carries the rule 49
// label and its rule 50 annotation lists the host of endpoint, the provider's
// spec.endpoint that the caller dials. The checks run on every read, through
// the watcher or the Reader path, so a label or annotation removed takes
// effect on the next request. They add no API call: the Secret is already in
// memory, and the host match walks the annotation without allocating.
func (k *KubeStore) providerCredential(ctx context.Context, endpoint, name, key string) (string, error) {
	ns := k.OperatorNamespace
	sec, err := k.secret(ctx, ns, name)
	if err != nil {
		return "", err
	}
	if !kaalmv1beta1.ProviderCredentialOptedIn(sec.Labels) {
		return "", fmt.Errorf("secret %s/%s does not carry the label %s: %q",
			ns, name, kaalmv1beta1.LabelProviderCredential, kaalmv1beta1.AnnotationTrue)
	}
	host := kaalmv1beta1.EndpointHost(endpoint)
	if !kaalmv1beta1.ProviderHostApproved(sec.Annotations, host) {
		return "", fmt.Errorf("secret %s/%s does not list the endpoint host %q in its %s annotation",
			ns, name, host, kaalmv1beta1.AnnotationProviderHosts)
	}
	return secretKey(sec, ns, name, key)
}

// ChannelByPath resolves path to the Ready AgentChannel registered at it,
// listing through ChannelPathIndex. The prefix defense (the path must begin
// with the channel's own /channels/{namespace}/ prefix) is enforced here,
// independent of the reconciler's InvalidPath status. When more than one
// Ready channel holds the path, which happens only until the reconciler marks
// the loser PathConflict, rule 15 picks the winner: the earliest
// creationTimestamp, and on a tie the lower name.
func (k *KubeStore) ChannelByPath(ctx context.Context, path string) (*kaalmv1beta1.AgentChannel, bool) {
	var channels kaalmv1beta1.AgentChannelList
	if err := k.Reader.List(ctx, &channels, client.MatchingFields{ChannelPathIndex: path}); err != nil {
		return nil, false
	}
	var winner *kaalmv1beta1.AgentChannel
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.Path() != path || !channelPathAllowed(ch) || !channelReady(ch) {
			continue
		}
		if winner == nil || channelPrecedes(ch, winner) {
			winner = ch
		}
	}
	return winner, winner != nil
}

// channelReady reports whether the channel's Ready condition is True.
func channelReady(ch *kaalmv1beta1.AgentChannel) bool {
	for _, c := range ch.Status.Conditions {
		if c.Type == kaalmv1beta1.ConditionReady && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// channelPrecedes reports whether a wins a rule 15 path conflict against b:
// the earlier creationTimestamp, and on a tie the lower name.
func channelPrecedes(a, b *kaalmv1beta1.AgentChannel) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// channelPathAllowed is the gateway-side half of validation rule 15.
func channelPathAllowed(ch *kaalmv1beta1.AgentChannel) bool {
	return strings.HasPrefix(ch.Spec.Path(), "/channels/"+ch.Namespace+"/")
}

// channelCredential reads one channel credential Secret from a user namespace
// and refuses it unless it carries the rule 45 opt-in label. The check runs
// on every read, even when a Role still grants the Secret: the reconciler
// shrinks the Role only on its next pass, and a watch established before the
// shrink can outlive it.
func (k *KubeStore) channelCredential(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	sec, err := k.secret(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if !kaalmv1beta1.ChannelCredentialOptedIn(sec.Labels) {
		return nil, fmt.Errorf("secret %s/%s does not carry the label %s: %q",
			namespace, name, kaalmv1beta1.LabelChannelCredential, kaalmv1beta1.AnnotationTrue)
	}
	return sec, nil
}

// SecretValue reads one key of a channel credential Secret in a user
// namespace. It refuses a Secret without the rule 45 opt-in label before it
// looks at any key.
func (k *KubeStore) SecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	sec, err := k.channelCredential(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return secretKey(sec, namespace, name, key)
}

// CallbackSecretValue reads a bearer callback token: SecretValue, plus rule
// 46, the Secret's kaalm.io/callback-hosts annotation must list host.
func (k *KubeStore) CallbackSecretValue(ctx context.Context, namespace, name, key, host string) (string, error) {
	sec, err := k.channelCredential(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	if !kaalmv1beta1.CallbackHostApproved(sec.Annotations, host) {
		return "", fmt.Errorf("secret %s/%s does not list the callbackUrl host %q in its %s annotation",
			namespace, name, host, kaalmv1beta1.AnnotationCallbackHosts)
	}
	return secretKey(sec, namespace, name, key)
}

// secretKey returns one non-empty key of a channel or provider credential
// Secret.
func secretKey(sec *corev1.Secret, namespace, name, key string) (string, error) {
	val, ok := sec.Data[key]
	if !ok || len(val) == 0 {
		return "", fmt.Errorf("key %q missing in Secret %s/%s", key, namespace, name)
	}
	return string(val), nil
}

// PodByIP resolves a source IP via the status.podIP cache index. The
// returned Pod shares memory with the informer cache and is read-only:
// the lookup runs once per authenticated request, and a deep copy of the
// Pod per request was a quarter of the gateway's allocations under the
// load baseline.
func (k *KubeStore) PodByIP(ctx context.Context, ip string) (*corev1.Pod, bool) {
	var pods corev1.PodList
	if err := k.Reader.List(ctx, &pods, client.MatchingFields{PodIPIndex: ip},
		client.UnsafeDisableDeepCopy); err != nil {
		return nil, false
	}
	return firstLivePod(&pods)
}

// PodByIPLive resolves a source IP with a live List against the apiserver,
// narrowed to namespace. status.podIP is an apiserver-supported Pod field
// selector, so the uncached reader passes the match through as a selector
// rather than listing the namespace.
func (k *KubeStore) PodByIPLive(ctx context.Context, namespace, ip string) (*corev1.Pod, bool) {
	if k.APIReader == nil {
		return nil, false
	}
	var pods corev1.PodList
	if err := k.APIReader.List(ctx, &pods,
		client.InNamespace(namespace), client.MatchingFields{PodIPIndex: ip}); err != nil {
		return nil, false
	}
	return firstLivePod(&pods)
}

// firstLivePod returns the first non-terminated Pod of a list. Terminated
// Pods are skipped because their IP may already be recycled.
func firstLivePod(pods *corev1.PodList) (*corev1.Pod, bool) {
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		return p, true
	}
	return nil, false
}
