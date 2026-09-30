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
	"reflect"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	}}
	for _, tc := range cases {
		got := fallbackIneligibility(tc.primary, index(tc.providers...), tc.callers)
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
	rec := record.NewFakeRecorder(10)
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
// when the set is unchanged or shrinks (#324). The condition message always
// lists the full current set.
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
