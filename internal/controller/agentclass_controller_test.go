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
	"errors"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A provider read that fails with anything but NotFound fails the pass: the
// error goes back to controller-runtime for a backoff retry, and the class
// keeps its stored status and sends no events. Before, the error was dropped
// and the class was reported valid.
func TestAgentClass_ProviderReadErrorFailsThePass(t *testing.T) {
	errInjected := errors.New("injected provider read failure")
	cases := []struct {
		name  string
		set   func(*kaalmv1beta1.AgentClassSpec)
		fails func(client.Object) bool
	}{
		{
			name: "ModelProvider",
			set: func(s *kaalmv1beta1.AgentClassSpec) {
				s.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "mp"}}
			},
			fails: func(o client.Object) bool { _, ok := o.(*kaalmv1beta1.ModelProvider); return ok },
		},
		{
			name: "ToolProvider",
			set: func(s *kaalmv1beta1.AgentClassSpec) {
				s.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tp"}}
			},
			fails: func(o client.Object) bool { _, ok := o.(*kaalmv1beta1.ToolProvider); return ok },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "read-err-" + strings.ToLower(tc.name)
			ac := &kaalmv1beta1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
				},
			}
			tc.set(&ac.Spec)
			ac.Status.Conditions = []metav1.Condition{{
				Type: kaalmv1beta1.ConditionReady, Status: metav1.ConditionFalse,
				Reason: kaalmv1beta1.ReasonInvalidReference, Message: "seeded",
				LastTransitionTime: metav1.Now(),
			}}
			c := classClientBuilder(t, ac).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tc.fails(obj) {
						return errInjected
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			rec := record.NewFakeRecorder(8)
			r := &AgentClassReconciler{Client: c, Recorder: rec}

			_, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
			if !errors.Is(err, errInjected) {
				t.Fatalf("Reconcile returned %v, want the injected read error", err)
			}

			var got kaalmv1beta1.AgentClass
			if err := c.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
				t.Fatal(err)
			}
			ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse ||
				ready.Reason != kaalmv1beta1.ReasonInvalidReference || ready.Message != "seeded" {
				t.Errorf("a failed pass changed Ready: %+v", ready)
			}
			if got.Status.ObservedGeneration != 0 {
				t.Errorf("a failed pass wrote observedGeneration %d", got.Status.ObservedGeneration)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("a failed pass emitted %q", events)
			}
		})
	}
}

// reconcileClass runs one AgentClassReconciler pass over the named class and
// returns the stored class.
func reconcileClass(t *testing.T, c client.Client, rec *record.FakeRecorder, name string) *kaalmv1beta1.AgentClass {
	t.Helper()
	r := &AgentClassReconciler{Client: c, Recorder: rec}
	if _, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatal(err)
	}
	var got kaalmv1beta1.AgentClass
	if err := c.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// Rule 51: a malformed allowedNamespaces entry makes the class Ready=False
// with InvalidNamespacePattern, names only that entry, and sends one Warning
// when the reason first appears. Fixing the entry makes the class Ready.
func TestAgentClass_MalformedNamespacePatternIsNotReady(t *testing.T) {
	const name = "ac-bad-pattern"
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
		Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
	}}
	ac.Spec.AllowedNamespaces = []string{"team-a", "team-["}
	c := classClientBuilder(t, ac).Build()
	rec := record.NewFakeRecorder(16)

	var events []string
	for range 2 {
		got := reconcileClass(t, c, rec, name)
		ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionFalse ||
			ready.Reason != kaalmv1beta1.ReasonInvalidNamespacePattern {
			t.Fatalf("Ready = %+v, want False/%s", ready, kaalmv1beta1.ReasonInvalidNamespacePattern)
		}
		if !strings.Contains(ready.Message, `"team-["`) || strings.Contains(ready.Message, `"team-a"`) {
			t.Errorf("Ready message %q must name the malformed entry and only it", ready.Message)
		}
		events = append(events, drainEvents(rec)...)
	}
	if got := withPrefix(events, "Warning "+kaalmv1beta1.ReasonInvalidNamespacePattern); len(got) != 1 {
		t.Fatalf("two failing passes emitted %d InvalidNamespacePattern events, want 1: %q", len(got), events)
	}

	var stored kaalmv1beta1.AgentClass
	if err := c.Get(ctxT(), types.NamespacedName{Name: name}, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.AllowedNamespaces = []string{"team-a", "team-b"}
	if err := c.Update(ctxT(), &stored); err != nil {
		t.Fatal(err)
	}
	got := reconcileClass(t, c, rec, name)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != kaalmv1beta1.ReasonAllReferencesResolved {
		t.Fatalf("after the fix, Ready = %+v, want True/%s", ready, kaalmv1beta1.ReasonAllReferencesResolved)
	}
}

// Rule 52: a malformed allowedImages entry turns the class Ready=False with
// InvalidImagePattern, names only that entry, and warns once.
func TestAgentClass_MalformedImagePatternIsNotReady(t *testing.T) {
	const name = "ac-bad-image-pattern"
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
		Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
	}}
	ac.Spec.Image.AllowedImages = []string{"registry.test/agents/*", "registry.test/["}
	c := classClientBuilder(t, ac).Build()
	rec := record.NewFakeRecorder(16)

	var events []string
	for range 2 {
		got := reconcileClass(t, c, rec, name)
		ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionFalse ||
			ready.Reason != kaalmv1beta1.ReasonInvalidImagePattern {
			t.Fatalf("Ready = %+v, want False/%s", ready, kaalmv1beta1.ReasonInvalidImagePattern)
		}
		if !strings.Contains(ready.Message, `"registry.test/["`) ||
			strings.Contains(ready.Message, `"registry.test/agents/*"`) {
			t.Errorf("Ready message %q must name the malformed entry and only it", ready.Message)
		}
		events = append(events, drainEvents(rec)...)
	}
	if got := withPrefix(events, "Warning "+kaalmv1beta1.ReasonInvalidImagePattern); len(got) != 1 {
		t.Fatalf("two failing passes emitted %d InvalidImagePattern events, want 1: %q", len(got), events)
	}

	var stored kaalmv1beta1.AgentClass
	if err := c.Get(ctxT(), types.NamespacedName{Name: name}, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Image.AllowedImages = []string{"registry.test/agents/*", "registry.test/tools/*"}
	if err := c.Update(ctxT(), &stored); err != nil {
		t.Fatal(err)
	}
	got := reconcileClass(t, c, rec, name)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != kaalmv1beta1.ReasonAllReferencesResolved {
		t.Fatalf("after the fix, Ready = %+v, want True/%s", ready, kaalmv1beta1.ReasonAllReferencesResolved)
	}
}

// InvalidImagePattern ranks below InvalidCIDR and InvalidNamespacePattern and
// above InvalidReference. The message lists every problem.
func TestAgentClass_ImagePatternReasonPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		set        func(*kaalmv1beta1.AgentClassSpec)
		reason     string
		substrings []string
	}{{
		name: "a bad CIDR beats a bad image pattern",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.Network.Egress.AllowedCIDRs = []string{"10.0.0.0/33"}
		},
		reason:     kaalmv1beta1.ReasonInvalidCIDR,
		substrings: []string{"10.0.0.0/33", `allowedImages entry "["`},
	}, {
		name: "a bad namespace pattern beats a bad image pattern",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.AllowedNamespaces = []string{"team-["}
		},
		reason:     kaalmv1beta1.ReasonInvalidNamespacePattern,
		substrings: []string{`allowedNamespaces entry "team-["`, `allowedImages entry "["`},
	}, {
		name: "a bad image pattern beats a missing provider",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "img-prec-ghost"}}
		},
		reason:     kaalmv1beta1.ReasonInvalidImagePattern,
		substrings: []string{"img-prec-ghost", `allowedImages entry "["`},
	}, {
		name: "a bad image pattern beats a malformed host",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.Network.Egress.AllowedHosts = []string{"Not_A_Host"}
		},
		reason:     kaalmv1beta1.ReasonInvalidImagePattern,
		substrings: []string{"Not_A_Host", `allowedImages entry "["`},
	}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "ac-image-prec-" + string(rune('a'+i))
			ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
				Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
			}}
			ac.Spec.Image.AllowedImages = []string{"["}
			tc.set(&ac.Spec)
			got := reconcileClass(t, classClientBuilder(t, ac).Build(), record.NewFakeRecorder(16), name)
			ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tc.reason {
				t.Fatalf("Ready = %+v, want False/%s", ready, tc.reason)
			}
			for _, s := range tc.substrings {
				if !strings.Contains(ready.Message, s) {
					t.Errorf("Ready message %q does not list %q", ready.Message, s)
				}
			}
		})
	}
}

// With several kinds of problem on one class, the reason follows a fixed
// precedence: InvalidCIDR, then InvalidNamespacePattern, then
// InvalidReference. The message lists every problem.
func TestAgentClass_NamespacePatternReasonPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		set        func(*kaalmv1beta1.AgentClassSpec)
		reason     string
		substrings []string
	}{{
		name: "a bad CIDR beats a bad pattern",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.Network.Egress.AllowedCIDRs = []string{"10.0.0.0/33"}
		},
		reason:     kaalmv1beta1.ReasonInvalidCIDR,
		substrings: []string{"10.0.0.0/33", `allowedNamespaces entry "["`},
	}, {
		name: "a bad pattern beats a missing provider",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "prec-ghost"}}
		},
		reason:     kaalmv1beta1.ReasonInvalidNamespacePattern,
		substrings: []string{"prec-ghost", `allowedNamespaces entry "["`},
	}, {
		name: "a bad pattern beats a malformed host",
		set: func(s *kaalmv1beta1.AgentClassSpec) {
			s.Network.Egress.AllowedHosts = []string{"Not_A_Host"}
		},
		reason:     kaalmv1beta1.ReasonInvalidNamespacePattern,
		substrings: []string{"Not_A_Host", `allowedNamespaces entry "["`},
	}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "ac-pattern-prec-" + string(rune('a'+i))
			ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
				Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
			}}
			ac.Spec.AllowedNamespaces = []string{"["}
			tc.set(&ac.Spec)
			got := reconcileClass(t, classClientBuilder(t, ac).Build(), record.NewFakeRecorder(16), name)
			ready := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tc.reason {
				t.Fatalf("Ready = %+v, want False/%s", ready, tc.reason)
			}
			for _, s := range tc.substrings {
				if !strings.Contains(ready.Message, s) {
					t.Errorf("Ready message %q does not list %q", ready.Message, s)
				}
			}
		})
	}
}
