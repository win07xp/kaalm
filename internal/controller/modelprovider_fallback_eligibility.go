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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// scanFallbackEligibility is the reconcile-time counterpart of the gateway's
// request-time FallbackIneligible check (staticallyIneligible in
// internal/gateway/fallback.go). It runs once the fallback tree passed the
// structural checks, and records the verdict in the FallbackIneligible
// condition. The result is advisory: Ready is not touched.
func (r *ModelProviderReconciler) scanFallbackEligibility(ctx context.Context, mp *kaalmv1beta1.ModelProvider) error {
	var findings []string
	if len(mp.Spec.Fallback) > 0 {
		callers, err := r.callerNamespaces(ctx, mp)
		if err != nil {
			return err
		}
		var list kaalmv1beta1.ModelProviderList
		// The items are the cache's own objects, only read here.
		if err := r.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
			return err
		}
		providers := make(map[string]*kaalmv1beta1.ModelProvider, len(list.Items))
		for i := range list.Items {
			providers[list.Items[i].Name] = &list.Items[i]
		}
		findings = fallbackIneligibility(mp, providers, callers)
	}
	r.setFallbackEligibility(mp, findings)
	return nil
}

// callerNamespaces returns the namespaces of the Agents and AgentTasks that
// reference the provider and that its allowedNamespaces admits, sorted: the
// namespaces whose requests can reach its fallbacks.
func (r *ModelProviderReconciler) callerNamespaces(ctx context.Context, mp *kaalmv1beta1.ModelProvider) ([]string, error) {
	seen := map[string]bool{}
	// Both lists hand back the cache's own objects; only their namespaces
	// are read.
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexProviderRef: mp.Name}, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	for i := range agents.Items {
		seen[agents.Items[i].Namespace] = true
	}
	var tasks kaalmv1beta1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexProviderRef: mp.Name}, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	for i := range tasks.Items {
		seen[tasks.Items[i].Namespace] = true
	}
	namespaces := make([]string, 0, len(seen))
	for ns := range seen {
		if namespaceAllowed(ns, mp.Spec.AllowedNamespaces) {
			namespaces = append(namespaces, ns)
		}
	}
	sort.Strings(namespaces)
	return namespaces, nil
}

// fallbackIneligibility walks the fallback tree the way the gateway does for
// one request, once per caller namespace and primary model, and applies the
// gateway's per-candidate checks: the candidate's allowedNamespaces admits
// the namespace, and its spec.models offers the model the walk carries to it
// (the primary's model, rewritten by each edge's modelMap on the way down).
// As in the gateway, an ineligible candidate's own fallbacks are not walked,
// a provider reached twice is walked once, and a missing provider is
// skipped (the structural check reports it). With no callers, only the
// model check runs. The findings are deduplicated and sorted.
func fallbackIneligibility(
	primary *kaalmv1beta1.ModelProvider, providers map[string]*kaalmv1beta1.ModelProvider, callers []string,
) []string {
	found := map[string]bool{}
	namespaces := callers
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	for _, ns := range namespaces {
		for _, m := range primary.Spec.Models {
			visited := map[string]bool{primary.Name: true}
			var walk func(parent *kaalmv1beta1.ModelProvider, parentModel string)
			walk = func(parent *kaalmv1beta1.ModelProvider, parentModel string) {
				for _, ref := range parent.Spec.Fallback {
					child := providers[ref.Name]
					if child == nil || visited[child.Name] {
						continue
					}
					visited[child.Name] = true
					model := parentModel
					if mapped := ref.ModelMap[parentModel]; mapped != "" {
						model = mapped
					}
					eligible := true
					if ns != "" && !namespaceAllowed(ns, child.Spec.AllowedNamespaces) {
						found[fmt.Sprintf("fallback %q does not admit namespace %q", child.Name, ns)] = true
						eligible = false
					}
					if findModel(child, model) == nil {
						found[fmt.Sprintf("fallback %q does not offer model %q (primary model %q)",
							child.Name, model, m.ID)] = true
						eligible = false
					}
					if eligible {
						walk(child, model)
					}
				}
			}
			walk(primary, m.ID)
		}
	}
	findings := make([]string, 0, len(found))
	for f := range found {
		findings = append(findings, f)
	}
	sort.Strings(findings)
	return findings
}

// setFallbackEligibility records the scan in the FallbackIneligible
// condition, whose message lists every current finding. It holds a
// FallbackIneligible Warning event, sent once finish writes the condition,
// that names only the findings the previous condition did not list. A pass
// with the same or fewer findings updates the message without an event.
// Clearing the findings sets the condition False without an event; a
// provider that never had findings gets no condition.
//
// DegradeTargetNotCheapest and MaxOutputTokensUnset warn only when their
// condition first turns True, because only an edit to the providers changes
// them. This scan's findings also grow when other people act: a new team's
// Agents start using the primary from a namespace a fallback does not admit.
// The platform team learns of that only from an event, so each added
// finding is announced; a fixed finding is not, since whoever fixed it
// already knows.
func (r *ModelProviderReconciler) setFallbackEligibility(mp *kaalmv1beta1.ModelProvider, findings []string) {
	prev := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
	if len(findings) == 0 {
		if prev != nil {
			apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
				Type:   kaalmv1beta1.ConditionFallbackIneligible,
				Status: metav1.ConditionFalse,
				Reason: kaalmv1beta1.ReasonFallbackCandidatesEligible,
			})
		}
		return
	}
	// prev points into the conditions slice, so diff before the set below
	// overwrites its message.
	added := addedFindings(prev, findings)
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type:    kaalmv1beta1.ConditionFallbackIneligible,
		Status:  metav1.ConditionTrue,
		Reason:  kaalmv1beta1.ReasonFallbackIneligible,
		Message: strings.Join(findings, findingsSeparator),
	})
	if len(added) > 0 {
		r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonFallbackIneligible,
			strings.Join(added, findingsSeparator))
	}
}

// findingsSeparator joins the sorted findings in the condition message.
const findingsSeparator = "; "

// addedFindings returns the findings, in order, that the previous
// FallbackIneligible condition did not list. An absent or False previous
// condition listed none.
func addedFindings(prev *metav1.Condition, findings []string) []string {
	listed := map[string]bool{}
	if prev != nil && prev.Status == metav1.ConditionTrue && prev.Message != "" {
		for _, f := range strings.Split(prev.Message, findingsSeparator) {
			listed[f] = true
		}
	}
	var added []string
	for _, f := range findings {
		if !listed[f] {
			added = append(added, f)
		}
	}
	return added
}
