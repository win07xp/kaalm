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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// eventsProvider is a ModelProvider that already carries its finalizer and
// has no status yet, with the probe off unless a case turns it on.
func eventsProvider(name string, mutate func(*kaalmv1beta1.ModelProvider)) *kaalmv1beta1.ModelProvider {
	mp := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Generation: 1,
			Finalizers: []string{kaalmv1beta1.ProviderFinalizer},
		},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://api.example.com",
			CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
			AllowedNamespaces: []string{"*"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
			HealthCheck:       &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false},
		},
	}
	if mutate != nil {
		mutate(mp)
	}
	return mp
}

// eventsProviderReconciler builds a ModelProviderReconciler over a fake
// client holding objs, whose status writes fail while conflicts is armed.
func eventsProviderReconciler(
	t *testing.T, conflicts *statusConflicts, health ProviderHealthChecker, objs ...client.Object,
) (*ModelProviderReconciler, *record.FakeRecorder) {
	t.Helper()
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
		WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.ModelProvider{}).
		WithIndex(&kaalmv1beta1.Agent{}, IndexProviderRef, byProviderRef).
		WithIndex(&kaalmv1beta1.AgentTask{}, IndexProviderRef, byProviderRef).
		WithInterceptorFuncs(conflicts.funcs()).
		Build()
	rec := record.NewFakeRecorder(32)
	if health == nil {
		health = newFakeHealth()
	}
	return &ModelProviderReconciler{
		Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: health,
	}, rec
}

// Each Ready=False reason a ModelProvider reports needs a person to fix it,
// so it is a Warning event on its rising edge, sent only after the
// status write that records it succeeds.
func TestModelProvider_ReadyFalseWarningsFollowTheStatusWrite(t *testing.T) {
	unpriced := func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Budget = hardBudgetSpec()
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "free-model"}}
	}
	cases := []struct {
		name      string
		mp        *kaalmv1beta1.ModelProvider
		extra     []client.Object
		health    ProviderProbeResult
		reason    string
		substring string
	}{{
		name:      "credentials missing",
		mp:        eventsProvider("ev-mp-missing", nil),
		reason:    kaalmv1beta1.ReasonCredentialsMissing,
		substring: "ev-mp-missing-key",
	}, {
		name: "rule 11: a fallback that does not exist",
		mp: eventsProvider("ev-mp-nofb", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "ev-mp-nofb-gone"}}
		}),
		reason:    kaalmv1beta1.ReasonFallbackIneligible,
		substring: `fallback provider "ev-mp-nofb-gone" does not exist`,
	}, {
		name: "rule 12: a fallback of an incompatible type",
		mp: eventsProvider("ev-mp-fbtype", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "ev-mp-fbtype-vertex"}}
		}),
		extra: []client.Object{eventsProvider("ev-mp-fbtype-vertex", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Type = kaalmv1beta1.ProviderTypeGoogleVertex
		})},
		reason:    kaalmv1beta1.ReasonFallbackIneligible,
		substring: "rule 12",
	}, {
		name: "rule 18: a degrade target outside the catalog",
		mp: eventsProvider("ev-mp-deg", func(mp *kaalmv1beta1.ModelProvider) {
			to := "no-such-model"
			mp.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{
				{AtPercent: 100, Action: "degrade", DegradeTo: &to},
			}
		}),
		reason:    kaalmv1beta1.ReasonInvalidDegradeTarget,
		substring: `degradeTo "no-such-model"`,
	}, {
		name: "rule 41: a modelMap key that is not a model",
		mp: eventsProvider("ev-mp-map", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "ev-mp-map-backup", ModelMap: map[string]string{"not-a-model": "m1"}},
			}
		}),
		extra:     []client.Object{eventsProvider("ev-mp-map-backup", nil)},
		reason:    kaalmv1beta1.ReasonInvalidModelMap,
		substring: `key "not-a-model"`,
	}, {
		name:      "rule 33: hard enforcement over an unpriced model",
		mp:        eventsProvider("ev-mp-unpriced", unpriced),
		reason:    kaalmv1beta1.ReasonHardBudgetUnpriced,
		substring: `model "free-model" is unpriced`,
	}, {
		name: "rule 51: a malformed allowedNamespaces pattern",
		mp: eventsProvider("ev-mp-nspattern", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.AllowedNamespaces = []string{"*", "["}
		}),
		reason:    kaalmv1beta1.ReasonInvalidNamespacePattern,
		substring: `"["`,
	}, {
		name: "the probe's rejected credential",
		mp: eventsProvider("ev-mp-rejected", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: true}
		}),
		health:    ProviderProbeResult{AuthFailed: true, Err: errString("401 invalid api key")},
		reason:    kaalmv1beta1.ReasonCredentialsInvalid,
		substring: "401 invalid api key",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{tc.mp}, tc.extra...)
			if tc.reason != kaalmv1beta1.ReasonCredentialsMissing {
				objs = append(objs, providerKey(tc.mp.Name))
			}
			health := newFakeHealth()
			health.set(tc.mp.Name, tc.health)
			conflicts := &statusConflicts{}
			r, rec := eventsProviderReconciler(t, conflicts, health, objs...)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.mp.Name}}

			ev := expectEventOnceAcrossConflict(t, r, rec, conflicts, req, "Warning "+tc.reason)
			if !strings.Contains(ev, tc.substring) {
				t.Errorf("event %q does not contain %q", ev, tc.substring)
			}
			var got kaalmv1beta1.ModelProvider
			if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil ||
				c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
				t.Errorf("Ready = %+v, want False/%s", c, tc.reason)
			}
		})
	}
}

// A validation reason that gives way to another is a new rising edge.
func TestModelProvider_NewReadyFalseReasonWarnsAgain(t *testing.T) {
	mp := eventsProvider("ev-mp-next", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "ev-mp-next-gone"}}
	})
	conflicts := &statusConflicts{}
	r, rec := eventsProviderReconciler(t, conflicts, nil, mp, providerKey("ev-mp-next"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-mp-next"}}
	expectEventOnceAcrossConflict(t, r, rec, conflicts, req, "Warning "+kaalmv1beta1.ReasonFallbackIneligible)

	var got kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Fallback = nil
	to := "no-such-model"
	got.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 100, Action: "degrade", DegradeTo: &to}}
	if err := r.Update(ctxT(), &got); err != nil {
		t.Fatal(err)
	}
	expectEventOnceAcrossConflict(t, r, rec, conflicts, req, "Warning "+kaalmv1beta1.ReasonInvalidDegradeTarget)
}

// The advisory findings (a condition that turns True while Ready stays
// True) are states too: one Warning on the rising edge, after the write.
func TestModelProvider_AdvisoryWarningsFollowTheStatusWrite(t *testing.T) {
	period := gateway.PeriodKey("monthly", time.Now())
	gwPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ev-margin-gw-0", Namespace: testOperatorNamespace, Labels: gatewayPodLabels,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	cases := []struct {
		name   string
		mp     *kaalmv1beta1.ModelProvider
		extra  []client.Object
		reason string
	}{{
		name: "a degrade target that is not the cheapest model",
		mp: eventsProvider("ev-mp-cost", func(mp *kaalmv1beta1.ModelProvider) {
			to := "pricey"
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{
				{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"},
				{ID: "pricey", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"},
			}
			mp.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{
				{AtPercent: 100, Action: "degrade", DegradeTo: &to},
			}
		}),
		reason: kaalmv1beta1.ReasonDegradeTargetNotCheapest,
	}, {
		name: "a fallback candidate that does not offer the model",
		mp: eventsProvider("ev-mp-elig", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "ev-mp-elig-backup"}}
		}),
		extra: []client.Object{eventsProvider("ev-mp-elig-backup", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "other"}}
		})},
		reason: kaalmv1beta1.ReasonFallbackIneligible,
	}, {
		name: "observed traffic that exceeded the boundary margin",
		mp: eventsProvider("ev-mp-margin", func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Budget = hardBudgetSpec()
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{
				{ID: "m", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "1.00"},
			}
		}),
		extra: []client.Object{gwPod, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: gateway.BudgetConfigMapName("ev-mp-margin"), Namespace: testOperatorNamespace,
			},
			Data: map[string]string{
				"ev-margin-gw-0": fmt.Sprintf(`{"period":%q,"team-a":"96.00","_marginExceeded":"true"}`, period),
			},
		}},
		reason: kaalmv1beta1.ReasonBoundaryMarginRaised,
	}, {
		name:   "a crossing into an anthropic model with no maxOutputTokens",
		mp:     eventsProvider("ev-mp-nomax", crossIntoAnthropic("ev-mp-nomax-claude")),
		extra:  []client.Object{anthropicBackup("ev-mp-nomax-claude", nil)},
		reason: kaalmv1beta1.ReasonMaxOutputTokensUnset,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{tc.mp, providerKey(tc.mp.Name)}, tc.extra...)
			conflicts := &statusConflicts{}
			r, rec := eventsProviderReconciler(t, conflicts, nil, objs...)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.mp.Name}}
			expectEventOnceAcrossConflict(t, r, rec, conflicts, req, "Warning "+tc.reason)
			var got kaalmv1beta1.ModelProvider
			if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil ||
				c.Status != metav1.ConditionTrue {
				t.Errorf("an advisory finding cleared Ready: %+v", c)
			}
		})
	}
}

// A failing probe is an occurrence, not a state: every failing pass emits
// ProviderUnhealthy, and client-go's aggregation folds the repeats into one
// event with a count, which keeps it visible for as long as the outage lasts.
func TestModelProvider_ProviderUnhealthyOnEveryFailingProbe(t *testing.T) {
	mp := eventsProvider("ev-mp-down", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: true}
	})
	health := newFakeHealth()
	health.set("ev-mp-down", ProviderProbeResult{Err: errString("upstream 503")})
	r, rec := eventsProviderReconciler(t, &statusConflicts{}, health, mp, providerKey("ev-mp-down"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-mp-down"}}
	for range 3 {
		if _, err := r.Reconcile(ctxT(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonProviderUnhealthy); len(got) != 3 {
		t.Fatalf("three failing probes emitted %d ProviderUnhealthy events, want 3", len(got))
	}
}

// crossIntoAnthropic gives an openai primary one fallback edge into the
// named anthropic provider, mapping m1 to claude.
func crossIntoAnthropic(backup string) func(*kaalmv1beta1.ModelProvider) {
	return func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
			{Name: backup, ModelMap: map[string]string{"m1": "claude"}},
		}
	}
}

// anthropicBackup is an anthropic provider offering claude, declaring
// maxOutputTokens when max is set.
func anthropicBackup(name string, max *int64) *kaalmv1beta1.ModelProvider {
	return eventsProvider(name, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Type = kaalmv1beta1.ProviderTypeAnthropic
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "claude", MaxOutputTokens: max}}
	})
}

// A crossing into anthropic models with no maxOutputTokens is an advisory
// state: the MaxOutputTokensUnset condition lists the models while Ready
// stays True, the Warning fires once on its rising edge, a pass that finds
// the same models stays quiet, and declaring maxOutputTokens sets the
// condition False without an event.
func TestModelProvider_MaxOutputTokensUnsetCondition(t *testing.T) {
	mp := eventsProvider("ev-mp-max", crossIntoAnthropic("ev-mp-max-claude"))
	r, rec := eventsProviderReconciler(t, &statusConflicts{}, nil,
		mp, providerKey("ev-mp-max"), anthropicBackup("ev-mp-max-claude", nil))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-mp-max"}}
	cond := func() *metav1.Condition {
		var got kaalmv1beta1.ModelProvider
		if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		return condition(got.Status.Conditions, kaalmv1beta1.ConditionMaxOutputTokensUnset)
	}
	prefix := "Warning " + kaalmv1beta1.ReasonMaxOutputTokensUnset
	for range 3 {
		if _, err := r.Reconcile(ctxT(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 1 ||
		!strings.Contains(got[0], "ev-mp-max-claude/claude") {
		t.Fatalf("three passes over the same models emitted %q, want one naming ev-mp-max-claude/claude", got)
	}
	if c := cond(); c == nil || c.Status != metav1.ConditionTrue ||
		c.Reason != kaalmv1beta1.ReasonMaxOutputTokensUnset || !strings.Contains(c.Message, "ev-mp-max-claude/claude") {
		t.Fatalf("MaxOutputTokensUnset = %+v, want True naming ev-mp-max-claude/claude", c)
	}

	// Declaring the ceiling clears the condition without an event.
	var backup kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), types.NamespacedName{Name: "ev-mp-max-claude"}, &backup); err != nil {
		t.Fatal(err)
	}
	limit := int64(8192)
	backup.Spec.Models[0].MaxOutputTokens = &limit
	if err := r.Update(ctxT(), &backup); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatal(err)
	}
	if c := cond(); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("after declaring maxOutputTokens the condition = %+v, want False", c)
	}
	if got := drainEvents(rec); len(got) != 0 {
		t.Fatalf("clearing the condition emitted %q", got)
	}

	// Removing it again is a new rising edge.
	backup.Spec.Models[0].MaxOutputTokens = nil
	if err := r.Update(ctxT(), &backup); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatal(err)
	}
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 1 {
		t.Fatalf("a new rising edge emitted %d events, want 1", len(got))
	}
}

// A provider with no crossing into anthropic gets no MaxOutputTokensUnset
// condition at all.
func TestModelProvider_NoCrossingNoMaxOutputTokensCondition(t *testing.T) {
	mp := eventsProvider("ev-mp-plain", nil)
	r, _ := eventsProviderReconciler(t, &statusConflicts{}, nil, mp, providerKey("ev-mp-plain"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-mp-plain"}}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatal(err)
	}
	var got kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionMaxOutputTokensUnset); c != nil {
		t.Fatalf("a provider with no crossing has MaxOutputTokensUnset = %+v", c)
	}
}
