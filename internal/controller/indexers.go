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

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// Field index keys. Cross-resource lookups (usage counts, reference tracking,
// re-enqueue on a referenced object's change) all go through these indexes rather
// than full list scans, so the controller stays cheap at the 1000-workload target
// (docs/src/controller/overview.md).
const (
	// IndexAgentClassRef indexes Agents and AgentTasks by spec.agentClassRef.name.
	IndexAgentClassRef = "spec.agentClassRef.name"
	// IndexProviderRef indexes Agents and AgentTasks by each
	// spec.providers[].providerRef.name (a multi-value index).
	IndexProviderRef = "spec.providers.providerRef.name"
	// IndexAllowedProviders indexes AgentClasses by each
	// spec.allowedProviders[].name.
	IndexAllowedProviders = "spec.allowedProviders.name"
)

// SetupIndexers registers the field indexers the reconcilers depend on. It must
// run before the reconcilers start.
func SetupIndexers(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()

	if err := idx.IndexField(ctx, &agentryv1alpha1.Agent{}, IndexAgentClassRef, func(o client.Object) []string {
		return []string{o.(*agentryv1alpha1.Agent).Spec.AgentClassRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &agentryv1alpha1.AgentTask{}, IndexAgentClassRef, func(o client.Object) []string {
		return []string{o.(*agentryv1alpha1.AgentTask).Spec.AgentClassRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &agentryv1alpha1.Agent{}, IndexProviderRef, func(o client.Object) []string {
		return providerRefNames(o.(*agentryv1alpha1.Agent).Spec.Providers)
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &agentryv1alpha1.AgentTask{}, IndexProviderRef, func(o client.Object) []string {
		return providerRefNames(o.(*agentryv1alpha1.AgentTask).Spec.Providers)
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &agentryv1alpha1.AgentClass{}, IndexAllowedProviders, func(o client.Object) []string {
		ac := o.(*agentryv1alpha1.AgentClass)
		names := make([]string, 0, len(ac.Spec.AllowedProviders))
		for _, p := range ac.Spec.AllowedProviders {
			names = append(names, p.Name)
		}
		return names
	}); err != nil {
		return err
	}
	return nil
}

func providerRefNames(refs []agentryv1alpha1.AgentProviderReference) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.ProviderRef.Name)
	}
	return names
}
