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
	"regexp"
	"sort"
	"strconv"
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
	var findings []fallbackFinding
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

// fallbackFinding is one finding of the fallback eligibility scan. key
// joins the raw values that make up the finding, so two findings never merge
// even when their quoted values in message read the same.
type fallbackFinding struct {
	key, message string
}

// fallbackIneligibility walks the fallback tree the way the gateway does for
// one request, once per caller namespace and primary model, and applies the
// gateway's per-candidate checks: the candidate's allowedNamespaces admits
// the namespace, and its spec.models offers the model the walk carries to it
// (the primary's model, rewritten by each edge's modelMap on the way down).
// As in the gateway, an ineligible candidate's own fallbacks are not walked,
// a provider reached twice is walked once, and a missing provider is
// skipped (the structural check reports it). With no callers, only the
// model check runs. The findings are deduplicated by their raw values and
// sorted by message, then by key.
func fallbackIneligibility(
	primary *kaalmv1beta1.ModelProvider, providers map[string]*kaalmv1beta1.ModelProvider, callers []string,
) []fallbackFinding {
	found := map[string]string{}
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
						found["ns\x00"+child.Name+"\x00"+ns] = fmt.Sprintf("fallback %s does not admit namespace %s",
							quotedEntry(child.Name), quotedEntry(ns))
						eligible = false
					}
					if findModel(child, model) == nil {
						found["model\x00"+child.Name+"\x00"+model+"\x00"+m.ID] = fmt.Sprintf(
							"fallback %s does not offer model %s (primary model %s)",
							quotedEntry(child.Name), quotedEntry(model), quotedEntry(m.ID))
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
	findings := make([]fallbackFinding, 0, len(found))
	for key, message := range found {
		findings = append(findings, fallbackFinding{key: key, message: message})
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].message != findings[j].message {
			return findings[i].message < findings[j].message
		}
		return findings[i].key < findings[j].key
	})
	return findings
}

// setFallbackEligibility records the scan in the FallbackIneligible
// condition, whose message lists the first maxListedEntries findings in
// sorted order and counts the rest, so it fits the condition schema however
// many caller namespaces a fallback does not admit. Clearing the findings
// sets the condition False without an event; a provider that never had
// findings gets no condition.
//
// DegradeTargetNotCheapest and MaxOutputTokensUnset warn only when their
// condition first turns True, because only an edit to the providers changes
// them. This scan's findings also grow when other people act: a new team's
// Agents start using the primary from a namespace a fallback does not admit.
// The platform team learns of that only from an event, so the pass holds a
// FallbackIneligible Warning, sent once finish writes the condition, when
// either:
//
//   - a finding is newly among the listed ones: the event names it;
//   - the total grows: the event starts with the new total, and when every
//     new finding is past the listed ones, the total is all it says.
//
// A fixed finding is not announced, since whoever fixed it already knows,
// and a finding past the listed ones is never named, so it is counted once
// in a higher total and never announced again. The total leads the note
// because the event recorder cuts a note at 1,024 bytes, and the total is
// the part that must survive. Both messages are read the same way, by
// listedFindings, so a steady set of findings never sends an event. With no
// status field to hold the full set, a fix and a new unlisted finding in the
// same pass leave the total unchanged and send nothing.
func (r *ModelProviderReconciler) setFallbackEligibility(mp *kaalmv1beta1.ModelProvider, findings []fallbackFinding) {
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
	msg := fallbackFindingsMessage(findings)
	// prev points into the conditions slice, so read it before the set below
	// overwrites its message.
	var prevEntries []string
	prevTotal := 0
	if prev != nil && prev.Status == metav1.ConditionTrue {
		prevEntries, prevTotal = listedFindings(prev.Message)
	}
	note := fallbackFindingsNote(prevEntries, prevTotal, msg)
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type:    kaalmv1beta1.ConditionFallbackIneligible,
		Status:  metav1.ConditionTrue,
		Reason:  kaalmv1beta1.ReasonFallbackIneligible,
		Message: msg,
	})
	if note != "" {
		r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonFallbackIneligible, kaalmv1beta1.ActionValidate, nil, note)
	}
}

// fallbackFindingsNote returns the FallbackIneligible event note for a pass
// that turns a message with prevEntries listed and prevTotal findings into
// msg, or "" when the pass sends no event (see setFallbackEligibility).
func fallbackFindingsNote(prevEntries []string, prevTotal int, msg string) string {
	curEntries, curTotal := listedFindings(msg)
	listed := make(map[string]bool, len(prevEntries))
	for _, e := range prevEntries {
		listed[e] = true
	}
	var added []string
	for _, e := range curEntries {
		if !listed[e] {
			added = append(added, e)
		}
	}
	if curTotal <= prevTotal {
		return strings.Join(added, findingsSeparator)
	}
	total := "1 finding in total"
	if curTotal != 1 {
		total = fmt.Sprintf("%d findings in total", curTotal)
	}
	if len(added) == 0 {
		return fmt.Sprintf("%s; the new findings are not among the %d the condition lists", total, len(curEntries))
	}
	return total + "; new: " + strings.Join(added, findingsSeparator)
}

// findingsSeparator joins the sorted findings in the condition message.
const findingsSeparator = "; "

// The count of the findings past the listed ones, which ends the
// FallbackIneligible message. Every finding starts with `fallback "`, which
// sorts before this text, and the count is added last in any case.
const (
	fallbackFindingsOneMore  = "fallback eligibility scan has 1 more finding, not listed"
	fallbackFindingsManyMore = "fallback eligibility scan has %d more findings, not listed"
)

var fallbackFindingsMoreRE = regexp.MustCompile(`^fallback eligibility scan has ([0-9]+) more findings?, not listed$`)

// fallbackFindingsMessage lists the first maxListedEntries findings, in
// order, and counts the rest, cut to fit a condition message.
func fallbackFindingsMessage(findings []fallbackFinding) string {
	var l entryList
	for _, f := range findings {
		l.add(f.key, f.message)
	}
	return cutConditionMessage(strings.Join(l.messages(fallbackFindingsOneMore, fallbackFindingsManyMore), findingsSeparator))
}

// listedFindings reads a FallbackIneligible message: the findings it lists,
// and the total number of findings, listed and counted. An empty message
// holds none.
func listedFindings(msg string) (entries []string, total int) {
	if msg == "" {
		return nil, 0
	}
	more := 0
	for _, part := range splitFindings(msg) {
		if m := fallbackFindingsMoreRE.FindStringSubmatch(part); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				more = n
				continue
			}
		}
		entries = append(entries, part)
	}
	return entries, len(entries) + more
}

// splitFindings splits a FallbackIneligible message into its findings. The
// values in a finding are %q-quoted, and a model id can hold the separator,
// so a separator inside a quoted value is not a boundary.
func splitFindings(msg string) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(msg); i++ {
		switch {
		case inQuote && msg[i] == '\\':
			i++
		case msg[i] == '"':
			inQuote = !inQuote
		case !inQuote && strings.HasPrefix(msg[i:], findingsSeparator):
			parts = append(parts, msg[start:i])
			i += len(findingsSeparator) - 1
			start = i + 1
		}
	}
	return append(parts, msg[start:])
}
