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
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The predicates below narrow the watches the ModelProvider, ToolProvider,
// and AgentClass reconcilers keep on the objects that reference them. An
// Agent or AgentTask writes its status many times over its life, and each
// write that reached these reconcilers re-ran a provider or class pass that
// reads nothing the write changed. Each predicate admits every create and
// delete (the reference counts and the delete holds follow them; a
// terminating referrer still counts until its final delete) and the updates
// that change what the reconciler reads.

// providerRefsChanged admits an Agent or AgentTask update that changes its
// spec.providers names: the ModelProvider reads its referrers (for the
// delete hold) and their namespaces (for the fallback eligibility scan),
// both keyed by those names. A workload's namespace never changes.
func providerRefsChanged() predicate.Predicate {
	return workloadChanged(func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return providerRefNames(w.Spec.Providers)
		case *kaalmv1beta1.AgentTask:
			return providerRefNames(w.Spec.Providers)
		}
		return nil
	})
}

// toolGrantsChanged admits an Agent or AgentTask update that changes the
// ToolProviders its spec.tools grants name: the ToolProvider reads only
// which workloads reference it, for the delete hold.
func toolGrantsChanged() predicate.Predicate {
	return workloadChanged(func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return toolGrantNames(w.Spec.Tools)
		case *kaalmv1beta1.AgentTask:
			return toolGrantNames(w.Spec.Tools)
		}
		return nil
	})
}

// classUsageChanged admits an Agent or AgentTask update that changes what
// the AgentClass counts: the class it names (agentsInUse, tasksInUse, and
// the delete hold) and, for an Agent, whether it is in a spec-drift
// replacement (agentsReplacing and agentsPendingReplacement).
func classUsageChanged() predicate.Predicate {
	return workloadChanged(func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return []string{w.Spec.AgentClassRef.Name, driftBucket(w)}
		case *kaalmv1beta1.AgentTask:
			return []string{w.Spec.AgentClassRef.Name}
		}
		return nil
	})
}

// driftBucket is the drift state the AgentClass counts an Agent in:
// Replacing, ReplacementPending, or neither ("").
func driftBucket(agent *kaalmv1beta1.Agent) string {
	switch r := podUpToDateReason(agent); r {
	case kaalmv1beta1.ReasonReplacing, kaalmv1beta1.ReasonReplacementPending:
		return r
	}
	return ""
}

// workloadChanged admits creates and deletes, and an update whose read
// values differ between the old and new object. An object of another type
// is admitted, so a misuse shows as extra passes, never as a lost one.
func workloadChanged(read func(client.Object) []string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, cur := read(e.ObjectOld), read(e.ObjectNew)
			if old == nil || cur == nil {
				return true
			}
			return !slices.Equal(old, cur)
		},
	}
}

// createOrDelete admits only creates and deletes. The AgentClass checks
// only that each provider it allows exists, so a provider's spec and status
// writes (spend folds, probe results) need not re-run every class.
func createOrDelete() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
