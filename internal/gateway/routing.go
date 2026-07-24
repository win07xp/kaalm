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
	"path"
	"strings"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// splitQualifiedModel splits {providerRef}/{modelId} on the FIRST slash.
func splitQualifiedModel(qualified string) (provider, model string, ok bool) {
	i := strings.Index(qualified, "/")
	if i <= 0 || i == len(qualified)-1 {
		return "", "", false
	}
	return qualified[:i], qualified[i+1:], true
}

// routeDenial classifies an authorization failure into its wire response.
type routeDenial struct {
	status  int
	errType string
	message string
}

// authorizeRoute walks the tenancy chain for the authenticated caller and
// returns the target provider, or a denial. The order is load-bearing:
// workload-level gates first (mTLS tier only), then the provider's namespace
// allowlist, and the model-existence check strictly last, so a namespace not
// authorized for a provider never learns which models it hosts.
func (s *Server) authorizeRoute(
	ctx context.Context, c *caller, providerName, modelID string,
) (*kaalmv1alpha1.ModelProvider, *routeDenial) {
	// Workload gates (mTLS tier). Gateway-only callers have no Agent,
	// AgentTask, or AgentClass: their chain is allowedNamespaces plus the
	// model list alone.
	if c.Workload != nil {
		refs, classRef, found := s.workloadProviders(ctx, c)
		if !found {
			return nil, &routeDenial{403, errAccessDenied,
				fmt.Sprintf("%s %q not found in namespace %q", c.Workload.Kind, c.Workload.Name, c.Namespace)}
		}
		if !containsProviderRef(refs, providerName) {
			return nil, &routeDenial{403, errAccessDenied,
				fmt.Sprintf("provider %q is not in the workload's spec.providers", providerName)}
		}
		class, ok := s.Store.ClassByName(ctx, classRef)
		if !ok {
			return nil, &routeDenial{403, errAccessDenied,
				fmt.Sprintf("AgentClass %q not found", classRef)}
		}
		allowed := false
		for _, ap := range class.Spec.AllowedProviders {
			if ap.Name == providerName {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, &routeDenial{403, errAccessDenied,
				fmt.Sprintf("provider %q is not in AgentClass %q allowedProviders", providerName, classRef)}
		}
	}

	provider, ok := s.Store.ProviderByName(ctx, providerName)
	if !ok {
		return nil, &routeDenial{400, errInvalidRequest,
			fmt.Sprintf("unknown provider %q", providerName)}
	}

	// Namespace allowlist BEFORE model existence.
	if !namespaceGlobAllowed(c.Namespace, provider.Spec.AllowedNamespaces) {
		return nil, &routeDenial{403, errAccessDenied,
			fmt.Sprintf("namespace %q is not allowed to use provider %q", c.Namespace, providerName)}
	}
	for _, m := range provider.Spec.Models {
		if m.ID == modelID {
			return provider, nil
		}
	}
	return nil, &routeDenial{400, errInvalidRequest,
		fmt.Sprintf("model %q is not offered by provider %q", modelID, providerName)}
}

// workloadProviders resolves the caller's workload resource to its provider
// references and class name.
func (s *Server) workloadProviders(ctx context.Context, c *caller) ([]kaalmv1alpha1.AgentProviderReference, string, bool) {
	switch c.Workload.Kind {
	case KindAgent:
		agent, ok := s.Store.AgentByName(ctx, c.Namespace, c.Workload.Name)
		if !ok {
			return nil, "", false
		}
		return agent.Spec.Providers, agent.Spec.AgentClassRef.Name, true
	case KindAgentTask:
		task, ok := s.Store.TaskByName(ctx, c.Namespace, c.Workload.Name)
		if !ok {
			return nil, "", false
		}
		return task.Spec.Providers, task.Spec.AgentClassRef.Name, true
	}
	return nil, "", false
}

func containsProviderRef(refs []kaalmv1alpha1.AgentProviderReference, name string) bool {
	for _, r := range refs {
		if r.ProviderRef.Name == name {
			return true
		}
	}
	return false
}

// namespaceGlobAllowed matches ns against exact names or path.Match globs. An
// empty list allows every namespace.
func namespaceGlobAllowed(ns string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, pattern := range allowed {
		if ok, err := path.Match(pattern, ns); err == nil && ok {
			return true
		}
	}
	return false
}
