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
	"reflect"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// eligProvider is a ModelProvider with the given models, allowed
// namespaces, and fallback edges.
func eligProvider(name string, models, namespaces []string, fallback ...kaalmv1beta1.FallbackReference) *kaalmv1beta1.ModelProvider {
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, m := range models {
		mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: m})
	}
	mp.Spec.AllowedNamespaces = namespaces
	mp.Spec.Fallback = fallback
	return mp
}

func index(mps ...*kaalmv1beta1.ModelProvider) map[string]*kaalmv1beta1.ModelProvider {
	m := map[string]*kaalmv1beta1.ModelProvider{}
	for _, mp := range mps {
		m[mp.Name] = mp
	}
	return m
}

func edge(name string, modelMap map[string]string) kaalmv1beta1.FallbackReference {
	return kaalmv1beta1.FallbackReference{Name: name, ModelMap: modelMap}
}

// The scan applies the gateway's request-time checks to every candidate a
// caller's request can reach: the candidate admits the caller's namespace,
// and offers the model the walk carries to it (the primary's model, mapped
// by each edge's modelMap on the way down).
func TestFallbackIneligibility(t *testing.T) {
	all := []string{"*"}
	longA := strings.Repeat("a", 300)
	cases := []struct {
		name      string
		primary   *kaalmv1beta1.ModelProvider
		providers []*kaalmv1beta1.ModelProvider
		callers   []string
		want      []string
	}{{
		name:      "eligible",
		primary:   eligProvider("p", []string{"m1"}, all, edge("b", nil)),
		providers: []*kaalmv1beta1.ModelProvider{eligProvider("b", []string{"m1"}, all)},
		callers:   []string{"team-a"},
	}, {
		name:      "missing model",
		primary:   eligProvider("p", []string{"m1", "m2"}, all, edge("b", nil)),
		providers: []*kaalmv1beta1.ModelProvider{eligProvider("b", []string{"m1"}, all)},
		want:      []string{`fallback "b" does not offer model "m2" (primary model "m2")`},
	}, {
		name:      "model map reaches an offered model",
		primary:   eligProvider("p", []string{"m1"}, all, edge("b", map[string]string{"m1": "b1"})),
		providers: []*kaalmv1beta1.ModelProvider{eligProvider("b", []string{"b1"}, all)},
	}, {
		name:    "mapping carries down the chain",
		primary: eligProvider("p", []string{"m1"}, all, edge("b", map[string]string{"m1": "b1"})),
		providers: []*kaalmv1beta1.ModelProvider{
			eligProvider("b", []string{"b1"}, all, edge("c", nil)),
			eligProvider("c", []string{"m1"}, all),
		},
		want: []string{`fallback "c" does not offer model "b1" (primary model "m1")`},
	}, {
		name:      "namespace not admitted",
		primary:   eligProvider("p", []string{"m1"}, all, edge("b", nil)),
		providers: []*kaalmv1beta1.ModelProvider{eligProvider("b", []string{"m1"}, []string{"team-a", "ops-*"})},
		callers:   []string{"team-a", "ops-1", "team-b"},
		want:      []string{`fallback "b" does not admit namespace "team-b"`},
	}, {
		name:    "an ineligible candidate's own fallbacks are unreachable",
		primary: eligProvider("p", []string{"m1"}, all, edge("b", nil)),
		providers: []*kaalmv1beta1.ModelProvider{
			eligProvider("b", []string{"other"}, all, edge("c", nil)),
			eligProvider("c", []string{"other"}, all),
		},
		want: []string{`fallback "b" does not offer model "m1" (primary model "m1")`},
	}, {
		name:    "a shared backup is checked once per walk",
		primary: eligProvider("p", []string{"m1"}, all, edge("b", nil), edge("c", nil)),
		providers: []*kaalmv1beta1.ModelProvider{
			eligProvider("b", []string{"m1"}, all, edge("d", map[string]string{"m1": "d1"})),
			eligProvider("c", []string{"m1"}, all, edge("d", nil)),
			eligProvider("d", []string{"d1"}, all),
		},
	}, {
		name:      "a missing provider is the structural check's to report",
		primary:   eligProvider("p", []string{"m1"}, all, edge("gone", nil)),
		providers: nil,
	}, {
		name:      "long model ids that share a quoted prefix stay two findings",
		primary:   eligProvider("p", []string{longA + "1", longA + "2"}, all, edge("b", nil)),
		providers: []*kaalmv1beta1.ModelProvider{eligProvider("b", []string{"m1"}, all)},
		want: []string{
			fmt.Sprintf("fallback %q does not offer model %s (primary model %s)", "b", quotedEntry(longA+"1"), quotedEntry(longA+"1")),
			fmt.Sprintf("fallback %q does not offer model %s (primary model %s)", "b", quotedEntry(longA+"2"), quotedEntry(longA+"2")),
		},
	}}
	for _, tc := range cases {
		var got []string
		for _, f := range fallbackIneligibility(tc.primary, index(tc.providers...), tc.callers) {
			got = append(got, f.message)
		}
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The reconciler reports ineligible candidates in the FallbackIneligible
// condition and a Warning event, without touching Ready. The event fires
// when findings are added, not on every pass.
func TestModelProvider_FallbackEligibilityScan(t *testing.T) {
	ctx := context.Background()
	primary := probedProvider("mp-elig", metav1.ConditionTrue, metav1.Now().Time)
	primary.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "m1"}}
	primary.Spec.AllowedNamespaces = []string{"team-*"}
	primary.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "mp-elig-backup"}}
	primary.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false}
	backup := eligProvider("mp-elig-backup", []string{"m1"}, []string{"team-a"})
	backup.Spec.Type = "openai"
	agentIn := func(name, ns string) *kaalmv1beta1.Agent {
		return &kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: kaalmv1beta1.AgentSpec{Providers: []kaalmv1beta1.AgentProviderReference{
				{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "mp-elig"}},
			}},
		}
	}
	byProviderRef := func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return providerRefNames(w.Spec.Providers)
		case *kaalmv1beta1.AgentTask:
			return providerRefNames(w.Spec.Providers)
		}
		return nil
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(primary, backup, providerKey("mp-elig"),
			agentIn("a", "team-a"), agentIn("b", "team-b"), agentIn("x", "outsider")).
		WithStatusSubresource(primary).
		WithIndex(&kaalmv1beta1.Agent{}, IndexProviderRef, byProviderRef).
		WithIndex(&kaalmv1beta1.AgentTask{}, IndexProviderRef, byProviderRef).
		Build()
	rec := events.NewFakeRecorder(10)
	r := &ModelProviderReconciler{
		Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "mp-elig"}}
	get := func() *kaalmv1beta1.ModelProvider {
		var mp kaalmv1beta1.ModelProvider
		if err := c.Get(ctx, req.NamespacedName, &mp); err != nil {
			t.Fatal(err)
		}
		return &mp
	}

	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	mp := get()
	cond := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		!strings.Contains(cond.Message, `does not admit namespace "team-b"`) {
		t.Fatalf("FallbackIneligible = %+v, want True naming team-b", cond)
	}
	if strings.Contains(cond.Message, "outsider") {
		t.Errorf("a namespace the primary does not admit is no caller of it: %q", cond.Message)
	}
	if !apimeta.IsStatusConditionTrue(mp.Status.Conditions, kaalmv1beta1.ConditionReady) {
		t.Error("an ineligible candidate is advisory and must not clear Ready")
	}
	if n := len(rec.Events); n != 1 {
		t.Fatalf("two passes over the same findings emitted %d events, want 1", n)
	}
	if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning FallbackIneligible") {
		t.Errorf("event = %q, want a FallbackIneligible Warning", ev)
	}

	// Admitting the namespace clears the condition without an event.
	backup = &kaalmv1beta1.ModelProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mp-elig-backup"}, backup); err != nil {
		t.Fatal(err)
	}
	backup.Spec.AllowedNamespaces = []string{"team-*"}
	if err := c.Update(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cond = apimeta.FindStatusCondition(get().Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("after the fix FallbackIneligible = %+v, want False", cond)
	}
	if n := len(rec.Events); n != 0 {
		t.Fatalf("clearing the findings emitted %d events, want 0", n)
	}

	// A finding after the condition went False is an added finding and warns.
	backup.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "other"}}
	if err := c.Update(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Events); n != 1 {
		t.Fatalf("a new finding emitted %d events, want 1", n)
	}
}

// The scan's findings grow when other people act (a new team's Agents use
// the primary from a namespace the fallback does not admit), so the Warning
// announces each added finding, naming only the new ones, and stays quiet
// when the set is unchanged or shrinks. With this few findings the
// condition message lists the full current set.
func TestModelProvider_FallbackIneligibleWarnsOnAddedFindings(t *testing.T) {
	mp := eventsProvider("ev-mp-grow", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "ev-mp-grow-backup"}}
	})
	backup := eventsProvider("ev-mp-grow-backup", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.AllowedNamespaces = []string{"team-a"}
	})
	agentIn := func(ns string) *kaalmv1beta1.Agent {
		return &kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns},
			Spec: kaalmv1beta1.AgentSpec{Providers: []kaalmv1beta1.AgentProviderReference{
				{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "ev-mp-grow"}},
			}},
		}
	}
	conflicts := &statusConflicts{}
	r, rec := eventsProviderReconciler(t, conflicts, nil,
		mp, backup, providerKey("ev-mp-grow"), agentIn("team-a"), agentIn("team-b"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-mp-grow"}}
	prefix := "Warning " + kaalmv1beta1.ReasonFallbackIneligible
	teamB := `fallback "ev-mp-grow-backup" does not admit namespace "team-b"`
	teamC := `fallback "ev-mp-grow-backup" does not admit namespace "team-c"`
	message := func() string {
		t.Helper()
		var got kaalmv1beta1.ModelProvider
		if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
		if c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("FallbackIneligible = %+v, want True", c)
		}
		return c.Message
	}
	pass := func() []string {
		t.Helper()
		if _, err := r.Reconcile(ctxT(), req); err != nil {
			t.Fatal(err)
		}
		return withPrefix(drainEvents(rec), prefix)
	}

	// First appearance: one Warning with the finding, sent only once the
	// status write succeeds.
	if ev := expectEventOnceAcrossConflict(t, r, rec, conflicts, req, prefix); !strings.Contains(ev, teamB) {
		t.Errorf("first event = %q, want it to name %s", ev, teamB)
	}

	// The same set sends nothing.
	if got := pass(); len(got) != 0 {
		t.Fatalf("a pass over the same findings emitted %q", got)
	}

	// An added finding warns once, naming only the new one; the condition
	// lists both.
	if err := r.Create(ctxT(), agentIn("team-c")); err != nil {
		t.Fatal(err)
	}
	ev := expectEventOnceAcrossConflict(t, r, rec, conflicts, req, prefix)
	if !strings.Contains(ev, teamC) || strings.Contains(ev, "team-b") {
		t.Errorf("event for the added finding = %q, want only %s", ev, teamC)
	}
	if got, want := message(), teamB+"; "+teamC; got != want {
		t.Errorf("condition message = %q, want %q", got, want)
	}

	// A removed finding sends nothing, and the condition message shrinks.
	var gone kaalmv1beta1.Agent
	if err := r.Get(ctxT(), types.NamespacedName{Namespace: "team-b", Name: "agent"}, &gone); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctxT(), &gone); err != nil {
		t.Fatal(err)
	}
	if got := pass(); len(got) != 0 {
		t.Fatalf("a shrinking set emitted %q", got)
	}
	if got := message(); got != teamC {
		t.Errorf("condition message after the removal = %q, want %q", got, teamC)
	}
}

// A model id can hold the separator the condition message joins findings
// with. The quoted value is one finding, so a second pass over the same
// finding must not announce it again.
func TestSetFallbackEligibility_SeparatorInModelIDNotReannounced(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r := &ModelProviderReconciler{}
	findings := fallbackIneligibility(
		eligProvider("p", []string{"a; b"}, nil, edge("b", nil)),
		index(eligProvider("b", nil, nil)), nil)
	r.setFallbackEligibility(mp, findings)
	if n := len(r.events.take(mp)); n != 1 {
		t.Fatalf("first pass held %d events, want 1", n)
	}
	r.setFallbackEligibility(mp, findings)
	if evs := r.events.take(mp); len(evs) != 0 {
		t.Fatalf("a pass over the same finding held %d events, want 0: %+v", len(evs), evs)
	}
}

// nsFindings returns the scan's findings for callers in the given
// namespaces, none of which the fallback admits: one finding per namespace,
// sorted by namespace.
func nsFindings(namespaces ...string) []fallbackFinding {
	return fallbackIneligibility(
		eligProvider("p", []string{"m"}, []string{"*"}, edge("b", nil)),
		index(eligProvider("b", []string{"m"}, []string{"nobody"})), namespaces)
}

// nsFinding is the listed text of the finding for a caller namespace.
func nsFinding(ns string) string {
	return fmt.Sprintf("fallback %q does not admit namespace %q", "b", ns)
}

// nsRange returns the namespaces ns-<from> through ns-<to>, two digits each,
// so they sort in number order.
func nsRange(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("ns-%02d", i))
	}
	return out
}

func fallbackIneligibleMessage(t *testing.T, mp *kaalmv1beta1.ModelProvider) string {
	t.Helper()
	c := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("FallbackIneligible = %+v, want True", c)
	}
	return c.Message
}

// The FallbackIneligible message lists the first ten findings in sorted
// order and counts the rest, so it fits the condition schema however many
// caller namespaces the fallback does not admit.
func TestSetFallbackEligibility_BoundsTheMessage(t *testing.T) {
	listed := func(namespaces []string) string {
		var parts []string
		for _, ns := range namespaces {
			parts = append(parts, nsFinding(ns))
		}
		return strings.Join(parts, "; ")
	}
	cases := []struct {
		name string
		ns   []string
		want string
	}{
		{"twelve", nsRange(1, 12), listed(nsRange(1, 10)) + "; fallback eligibility scan has 2 more findings, not listed"},
		{"eleven", nsRange(1, 11), listed(nsRange(1, 10)) + "; fallback eligibility scan has 1 more finding, not listed"},
		{"ten", nsRange(1, 10), listed(nsRange(1, 10))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
			r := &ModelProviderReconciler{}
			r.setFallbackEligibility(mp, nsFindings(tc.ns...))
			r.events.take(mp)
			got := fallbackIneligibleMessage(t, mp)
			if got != tc.want {
				t.Errorf("message =\n %q\nwant\n %q", got, tc.want)
			}
			if len(got) > conditionMessageLimit {
				t.Errorf("message is %d bytes", len(got))
			}
		})
	}
}

// The FallbackIneligible Warning names the findings that are new among the
// listed ones and, when the total grows, starts with the new total. A
// finding past the listed ten shows only as a higher total, once.
func TestSetFallbackEligibility_Events(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r := &ModelProviderReconciler{}
	listed := func(namespaces ...string) string {
		var parts []string
		for _, ns := range namespaces {
			parts = append(parts, nsFinding(ns))
		}
		return strings.Join(parts, "; ")
	}
	steps := []struct {
		name string
		ns   []string
		want []string // the held event notes
	}{{
		name: "zero to twelve names the listed ten after the total",
		ns:   nsRange(1, 12),
		want: []string{"12 findings in total; new: " + listed(nsRange(1, 10)...)},
	}, {
		name: "the same twelve send nothing",
		ns:   nsRange(1, 12),
	}, {
		name: "a new finding past the listed ten gives only the total",
		ns:   nsRange(1, 13),
		want: []string{"13 findings in total; the new findings are not among the 10 the condition lists"},
	}, {
		name: "the unlisted finding is not announced again",
		ns:   nsRange(1, 13),
	}, {
		name: "a new finding among the listed ten is named after the total",
		ns:   nsRange(0, 13),
		want: []string{"14 findings in total; new: " + listed("ns-00")},
	}, {
		name: "a fixed listed finding names the one that moved into the list",
		ns:   nsRange(1, 13),
		want: []string{listed("ns-10")},
	}, {
		name: "fixed unlisted findings send nothing",
		ns:   nsRange(1, 11),
	}}
	for _, s := range steps {
		r.setFallbackEligibility(mp, nsFindings(s.ns...))
		var got []string
		for _, ev := range r.events.take(mp) {
			got = append(got, ev.message)
		}
		if !reflect.DeepEqual(got, s.want) {
			t.Errorf("%s:\n got %q\nwant %q", s.name, got, s.want)
		}
	}

	// A short list: the total still leads when it grows.
	short := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "q"}}
	r.setFallbackEligibility(short, nsFindings("a"))
	if evs := r.events.take(short); len(evs) != 1 || evs[0].message != "1 finding in total; new: "+listed("a") {
		t.Errorf("first finding held %+v", evs)
	}
	r.setFallbackEligibility(short, nsFindings("a", "b"))
	if evs := r.events.take(short); len(evs) != 1 || evs[0].message != "2 findings in total; new: "+listed("b") {
		t.Errorf("added finding held %+v", evs)
	}
}

// fallbackPass runs one scan pass over findings for callers in namespaces
// and returns the notes of the events it holds.
func fallbackPass(r *ModelProviderReconciler, mp *kaalmv1beta1.ModelProvider, namespaces ...string) []string {
	r.setFallbackEligibility(mp, nsFindings(namespaces...))
	var notes []string
	for _, ev := range r.events.take(mp) {
		notes = append(notes, ev.message)
	}
	return notes
}

// A fix and a new finding in the same pass leave the total unchanged. The
// new finding is still named, even when neither finding is listed.
func TestSetFallbackEligibility_FixPlusNewUnlistedFindingIsNamed(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "uid-p"}}
	r := &ModelProviderReconciler{}
	fallbackPass(r, mp, nsRange(1, 12)...)
	got := fallbackPass(r, mp, append(nsRange(1, 11), "ns-13")...)
	want := []string{"new: " + nsFinding("ns-13")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fix plus a new unlisted finding held\n %q\nwant\n %q", got, want)
	}
}

// Many long model ids that a fallback does not offer still let the status
// write through: FallbackIneligible lists the first ten findings and counts
// the rest, and Ready is recorded at the current generation.
func TestModelProvider_LongFallbackFindingsFitStatus(t *testing.T) {
	const name = "mp-long-findings"
	const child = name + "-fb"
	mkSecret(t, name+"-key")
	mkSecret(t, child+"-key")
	mkProvider(t, child, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "m"}}
	})
	mkProvider(t, name, func(mp *kaalmv1beta1.ModelProvider) {
		for _, id := range longEntries("model-", "a") {
			mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: id})
		}
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: child}}
	})
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &mp); err != nil {
			return err
		}
		ready := condition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || mp.Status.ObservedGeneration != mp.Generation {
			return fmt.Errorf("Ready = %+v, want True at generation %d", ready, mp.Generation)
		}
		c := condition(mp.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible)
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("FallbackIneligible = %+v, want True", c)
		}
		if len(c.Message) > conditionMessageLimit {
			return fmt.Errorf("FallbackIneligible message is %d bytes", len(c.Message))
		}
		if !strings.HasSuffix(c.Message, "fallback eligibility scan has 2 more findings, not listed") {
			return fmt.Errorf("FallbackIneligible message does not end in the count")
		}
		return nil
	})
}
