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
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// Field index keys. Cross-resource lookups (usage counts, reference tracking,
// re-enqueue on a referenced object's change) go through these indexes rather
// than full list scans, so the controller stays cheap at the 1000-workload target
// (docs/src/controller/overview.md). The check that releases a delete also
// lists from the API server when the cache shows no referrer (deleteReferrers
// in deletion_blocked.go); overview.md names the other exceptions.
const (
	// IndexAgentClassRef indexes Agents and AgentTasks by spec.agentClassRef.name.
	IndexAgentClassRef = "spec.agentClassRef.name"
	// IndexProviderRef indexes Agents and AgentTasks by each
	// spec.providers[].providerRef.name (a multi-value index).
	IndexProviderRef = "spec.providers.providerRef.name"
	// IndexAllowedProviders indexes AgentClasses by each
	// spec.allowedProviders[].name.
	IndexAllowedProviders = "spec.allowedProviders.name"
	// IndexToolProviderRef indexes Agents and AgentTasks by each
	// spec.tools[].providerRef.name (a multi-value index).
	IndexToolProviderRef = "spec.tools.providerRef.name"
	// IndexAllowedToolProviders indexes AgentClasses by each
	// spec.allowedToolProviders[].name.
	IndexAllowedToolProviders = "spec.allowedToolProviders.name"
	// IndexChannelPath indexes AgentChannels by the path of the block their
	// type selects (spec.webhook.path, spec.discord.path, or
	// spec.whatsapp.path). A channel with no path is not indexed.
	IndexChannelPath = "spec.path"
	// IndexChannelAgentRef indexes AgentChannels by spec.agentRef.name, the
	// Agent they bind in their own namespace.
	IndexChannelAgentRef = "spec.agentRef.name"
	// IndexChannelSecretRef indexes AgentChannels by each Secret name their
	// credentials reference, the names authSecretNames returns (a
	// multi-value index).
	IndexChannelSecretRef = "spec.secretRef.name"
	// IndexAsyncChannel indexes the gateway's kaalm-async-* ConfigMaps by
	// the channel their labels name, as "namespace/name", so a channel's
	// prune reads only its own records.
	IndexAsyncChannel = "asyncRecord.channel"
	// IndexEnvSecretRef indexes Agents and AgentTasks by each Secret name
	// their spec.env reads, the names envSecretRefs returns (a multi-value
	// index). An empty name is not indexed: the rule 48 check reports it as
	// InvalidReference, and no Secret can be watched for it.
	IndexEnvSecretRef = "spec.env.secretKeyRef.name"
	// IndexClassPullSecret indexes AgentClasses by each
	// spec.image.imagePullSecrets[].name (a multi-value index).
	IndexClassPullSecret = "spec.image.imagePullSecrets.name"
)

// SetupIndexers registers the field indexers the reconcilers depend on. It must
// run before the reconcilers start.
func SetupIndexers(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()

	for _, r := range referrerFieldIndexes {
		if err := idx.IndexField(ctx, r.obj, r.f.key, r.f.extract); err != nil {
			return err
		}
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.AgentChannel{}, IndexChannelAgentRef, channelAgentRefIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.AgentChannel{}, IndexChannelSecretRef, channelSecretRefIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.Agent{}, IndexEnvSecretRef, envSecretRefIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.AgentTask{}, IndexEnvSecretRef, envSecretRefIndex); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &kaalmv1beta1.AgentClass{}, IndexClassPullSecret, classPullSecretIndex); err != nil {
		return err
	}
	return nil
}

// fieldIndex pairs an index key with its extractor, so a lookup that
// cannot use the index (a read from the API server) applies the same rule.
type fieldIndex struct {
	key     string
	extract client.IndexerFunc
}

// The referrer indexes: what pins a ModelProvider, ToolProvider, or
// AgentClass against deletion.
var (
	agentClassRefField        = fieldIndex{IndexAgentClassRef, agentClassRefIndex}
	providerRefField          = fieldIndex{IndexProviderRef, providerRefIndex}
	toolProviderRefField      = fieldIndex{IndexToolProviderRef, toolProviderRefIndex}
	allowedProvidersField     = fieldIndex{IndexAllowedProviders, allowedProvidersIndex}
	allowedToolProvidersField = fieldIndex{IndexAllowedToolProviders, allowedToolProvidersIndex}
)

// referrerFieldIndexes lists each referrer index with the kind it indexes. The
// manager and the test fakes register the same table, so a change to what
// counts as a referrer reaches both.
var referrerFieldIndexes = []struct {
	obj client.Object
	f   fieldIndex
}{
	{&kaalmv1beta1.Agent{}, agentClassRefField},
	{&kaalmv1beta1.AgentTask{}, agentClassRefField},
	{&kaalmv1beta1.Agent{}, providerRefField},
	{&kaalmv1beta1.AgentTask{}, providerRefField},
	{&kaalmv1beta1.AgentClass{}, allowedProvidersField},
	{&kaalmv1beta1.Agent{}, toolProviderRefField},
	{&kaalmv1beta1.AgentTask{}, toolProviderRefField},
	{&kaalmv1beta1.AgentClass{}, allowedToolProvidersField},
}

// agentClassRefIndex is the IndexAgentClassRef extractor for Agents and
// AgentTasks.
func agentClassRefIndex(o client.Object) []string {
	switch w := o.(type) {
	case *kaalmv1beta1.Agent:
		return []string{w.Spec.AgentClassRef.Name}
	case *kaalmv1beta1.AgentTask:
		return []string{w.Spec.AgentClassRef.Name}
	}
	return nil
}

// providerRefIndex is the IndexProviderRef extractor for Agents and
// AgentTasks.
func providerRefIndex(o client.Object) []string {
	switch w := o.(type) {
	case *kaalmv1beta1.Agent:
		return providerRefNames(w.Spec.Providers)
	case *kaalmv1beta1.AgentTask:
		return providerRefNames(w.Spec.Providers)
	}
	return nil
}

// toolProviderRefIndex is the IndexToolProviderRef extractor for Agents and
// AgentTasks.
func toolProviderRefIndex(o client.Object) []string {
	switch w := o.(type) {
	case *kaalmv1beta1.Agent:
		return toolGrantNames(w.Spec.Tools)
	case *kaalmv1beta1.AgentTask:
		return toolGrantNames(w.Spec.Tools)
	}
	return nil
}

// allowedProvidersIndex is the IndexAllowedProviders extractor.
func allowedProvidersIndex(o client.Object) []string {
	if ac, ok := o.(*kaalmv1beta1.AgentClass); ok {
		return localRefNames(ac.Spec.AllowedProviders)
	}
	return nil
}

// allowedToolProvidersIndex is the IndexAllowedToolProviders extractor.
func allowedToolProvidersIndex(o client.Object) []string {
	if ac, ok := o.(*kaalmv1beta1.AgentClass); ok {
		return localRefNames(ac.Spec.AllowedToolProviders)
	}
	return nil
}

// envSecretRefIndex is the IndexEnvSecretRef extractor for Agents and
// AgentTasks.
func envSecretRefIndex(o client.Object) []string {
	var env []corev1.EnvVar
	switch w := o.(type) {
	case *kaalmv1beta1.Agent:
		env = w.Spec.Env
	case *kaalmv1beta1.AgentTask:
		env = w.Spec.Env
	default:
		return nil
	}
	refs := envSecretRefs(env)
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return names
}

// classPullSecretIndex is the IndexClassPullSecret extractor.
func classPullSecretIndex(o client.Object) []string {
	ac, ok := o.(*kaalmv1beta1.AgentClass)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(ac.Spec.Image.ImagePullSecrets))
	for _, r := range ac.Spec.Image.ImagePullSecrets {
		names = append(names, r.Name)
	}
	return names
}

func localRefNames(refs []kaalmv1beta1.LocalObjectReference) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return names
}

// asyncChannelIndex is the IndexAsyncChannel extractor. A record without
// both channel labels, or a ConfigMap that is not an async record, is not
// indexed.
func asyncChannelIndex(o client.Object) []string {
	if !strings.HasPrefix(o.GetName(), "kaalm-async-") {
		return nil
	}
	ns, name := o.GetLabels()[kaalmv1beta1.LabelChannelNamespace], o.GetLabels()[kaalmv1beta1.LabelChannelName]
	if ns == "" || name == "" {
		return nil
	}
	return []string{ns + "/" + name}
}

// channelPathIndex is the IndexChannelPath extractor.
func channelPathIndex(o client.Object) []string {
	if path := o.(*kaalmv1beta1.AgentChannel).Spec.Path(); path != "" {
		return []string{path}
	}
	return nil
}

// channelAgentRefIndex is the IndexChannelAgentRef extractor.
func channelAgentRefIndex(o client.Object) []string {
	return []string{o.(*kaalmv1beta1.AgentChannel).Spec.AgentRef.Name}
}

// channelSecretRefIndex is the IndexChannelSecretRef extractor.
func channelSecretRefIndex(o client.Object) []string {
	return authSecretNames(o.(*kaalmv1beta1.AgentChannel))
}

func providerRefNames(refs []kaalmv1beta1.AgentProviderReference) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.ProviderRef.Name)
	}
	return names
}

func toolGrantNames(grants []kaalmv1beta1.AgentToolGrant) []string {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, g.ProviderRef.Name)
	}
	return names
}
