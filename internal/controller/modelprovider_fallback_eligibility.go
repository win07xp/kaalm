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
	"sync"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
// The platform team learns of that only from an event, so the pass derives
// a FallbackIneligible Warning, sent once finish stores the condition, that
// names every new finding, listed in the condition or not, and starts with
// the new total when the total grows. A fixed finding is not announced,
// since whoever fixed it already knows, and a steady set sends nothing. The
// total leads the note because the event recorder cuts a note at 1,024
// bytes, and the total is the part that must survive.
//
// The condition message cannot hold the full set, so the reconciler keeps
// the last one in memory (fallbackSeen). A stored condition that is absent
// or not True says the last set was empty, which stands in for the memory
// on a new provider's first scan. The first pass after a restart or a leader
// change, over a stored condition that lists findings, has neither and
// compares against the stored message instead (see fallbackListedNote).
func (r *ModelProviderReconciler) setFallbackEligibility(mp *kaalmv1beta1.ModelProvider, findings []fallbackFinding) {
	known, remembered := r.fallbackSeen.lookup(mp)
	prev := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
	if !remembered && (prev == nil || prev.Status != metav1.ConditionTrue) {
		known, remembered = nil, true
	}
	if len(findings) == 0 {
		r.fallbackSeen.stage(mp, findings, "")
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
	if remembered {
		// The memory holds the note, not r.events: a pass that names a new
		// finding the condition does not list leaves the status as stored,
		// and finish must still send it (see fallbackFindingsMemory).
		r.fallbackSeen.stage(mp, findings, fallbackNewNote(known, findings))
	} else {
		r.fallbackSeen.stage(mp, findings, "")
		// prev, True here, points into the conditions slice, so read it
		// before the set below overwrites its message.
		prevEntries, prevTotal := listedFindings(prev.Message)
		if note := fallbackListedNote(prevEntries, prevTotal, msg); note != "" {
			r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonFallbackIneligible, kaalmv1beta1.ActionValidate, nil, note)
		}
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type:    kaalmv1beta1.ConditionFallbackIneligible,
		Status:  metav1.ConditionTrue,
		Reason:  kaalmv1beta1.ReasonFallbackIneligible,
		Message: msg,
	})
}

// sendFallbackNote sends the FallbackIneligible Warning that settle returns
// for a stored pass.
func (r *ModelProviderReconciler) sendFallbackNote(mp *kaalmv1beta1.ModelProvider, note string) {
	if note == "" || r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(mp, nil, corev1.EventTypeWarning, kaalmv1beta1.ReasonFallbackIneligible,
		kaalmv1beta1.ActionValidate, "%s", note)
}

// fallbackNewNote returns the FallbackIneligible event note for a pass whose
// previous findings, by key, are known: it names every finding not in known,
// in order. A nil known is an empty set. It returns "" when no finding is
// new.
func fallbackNewNote(known map[string]struct{}, findings []fallbackFinding) string {
	var added []string
	for _, f := range findings {
		if _, ok := known[f.key]; !ok {
			added = append(added, f.message)
		}
	}
	if len(added) == 0 {
		return ""
	}
	note := "new: " + strings.Join(added, findingsSeparator)
	if len(findings) > len(known) {
		note = fallbackTotal(len(findings)) + "; " + note
	}
	return note
}

// fallbackListedNote returns the FallbackIneligible event note for a pass
// with no remembered findings, which turns a message with prevEntries
// listed and prevTotal findings into msg, or "" when the pass sends no
// event. It names the findings newly among the listed ones as "now listed",
// not "new": one may only have moved into the list when a listed one was
// fixed. This pass held its note in r.events, which finish drops when it
// finds the status already stored: a note derived from the stored message
// is new only when the message changed.
func fallbackListedNote(prevEntries []string, prevTotal int, msg string) string {
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
	var note string
	if len(added) > 0 {
		note = "now listed: " + strings.Join(added, findingsSeparator)
	}
	if curTotal <= prevTotal {
		return note
	}
	if len(added) == 0 {
		note = fmt.Sprintf("the new findings are not among the %d the condition lists", len(curEntries))
	}
	return fallbackTotal(curTotal) + "; " + note
}

// fallbackTotal is the total that leads a FallbackIneligible event note.
func fallbackTotal(n int) string {
	if n == 1 {
		return "1 finding in total"
	}
	return fmt.Sprintf("%d findings in total", n)
}

// fallbackFindingsMemory holds, per ModelProvider, the keys of the findings
// of its last recorded scan, so an event can name a new finding the
// condition message does not list. A pass stages its set with the note it
// derived from the memory, and finish commits the set and sends the note
// only when the pass's status is stored, whether the pass wrote it or found
// it already stored. A new finding the condition does not list leaves the
// message unchanged, so its pass finds the status stored; a pass that
// stored the set before moved the memory forward, so a repeat pass finds
// nothing new. After a failed status write the next pass compares against
// the same set again and sends the event the failed pass dropped.
//
// Entries are indexed by name, so the NotFound path, which has no UID, can
// drop them, and an entry is used only by the provider with the UID that
// recorded it, so a provider recreated under the same name starts with no
// memory. Each provider holds at most one entry, dropped when the provider
// is deleted. The zero value is ready to use.
type fallbackFindingsMemory struct {
	mu      sync.Mutex
	known   map[string]fallbackFindingsSet
	pending map[string]fallbackFindingsSet
}

type fallbackFindingsSet struct {
	uid  types.UID
	keys map[string]struct{}
	// note is the pass's event note, held while the set is pending.
	note string
}

// lookup returns the provider's remembered finding keys, and whether there
// are any; a provider whose findings dropped to zero has an empty set.
func (m *fallbackFindingsMemory) lookup(mp *kaalmv1beta1.ModelProvider) (map[string]struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.known[mp.Name]
	if !ok || s.uid != mp.UID {
		return nil, false
	}
	return s.keys, true
}

// stage holds the pass's findings and event note until settle.
func (m *fallbackFindingsMemory) stage(mp *kaalmv1beta1.ModelProvider, findings []fallbackFinding, note string) {
	keys := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		keys[f.key] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = map[string]fallbackFindingsSet{}
	}
	m.pending[mp.Name] = fallbackFindingsSet{uid: mp.UID, keys: keys, note: note}
}

// settle drops the pass's staged findings. When stored is true, the pass's
// status is in the API server: it remembers them and returns the pass's
// event note to send.
func (m *fallbackFindingsMemory) settle(mp *kaalmv1beta1.ModelProvider, stored bool) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.pending[mp.Name]
	if !ok {
		return ""
	}
	delete(m.pending, mp.Name)
	if !stored {
		return ""
	}
	if m.known == nil {
		m.known = map[string]fallbackFindingsSet{}
	}
	note := s.note
	s.note = ""
	m.known[mp.Name] = s
	return note
}

// forget drops the provider's remembered findings once it is deleted.
func (m *fallbackFindingsMemory) forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.known, name)
	delete(m.pending, name)
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
