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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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
			rec := events.NewFakeRecorder(8)
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
func reconcileClass(t *testing.T, c client.Client, rec *events.FakeRecorder, name string) *kaalmv1beta1.AgentClass {
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
	rec := events.NewFakeRecorder(16)

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
	rec := events.NewFakeRecorder(16)

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
			got := reconcileClass(t, classClientBuilder(t, ac).Build(), events.NewFakeRecorder(16), name)
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
			got := reconcileClass(t, classClientBuilder(t, ac).Build(), events.NewFakeRecorder(16), name)
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

// A pass that finds the class status already stored makes no write, and a
// Ready=False class sends its Warning once, from the pass that wrote it.
func TestAgentClass_UnchangedPassSkipsStatusWrite(t *testing.T) {
	const name = "ac-no-op"
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
		Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
	}}
	ac.Spec.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "ac-no-op-missing"}}
	writes := &statusWrites{}
	c := classClientBuilder(t, ac).WithInterceptorFuncs(writes.funcs()).Build()
	rec := events.NewFakeRecorder(16)

	first := reconcileClass(t, c, rec, name)
	second := reconcileClass(t, c, rec, name)
	if n := writes.count(); n != 1 {
		t.Errorf("two passes over an unchanged class made %d status writes, want 1", n)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion moved from %s to %s on an unchanged pass", first.ResourceVersion, second.ResourceVersion)
	}
	if got := withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonInvalidReference); len(got) != 1 {
		t.Errorf("two passes sent %d InvalidReference Warnings, want 1", len(got))
	}
}

// Rules 51 and 52: long lists of long malformed entries in both fields still
// turn Ready False. Each field lists its first entries and counts the rest,
// so the message fits the condition schema and the status write is accepted.
func TestAgentClass_LongPatternListsTurnReadyFalse(t *testing.T) {
	const name = "ac-long-patterns"
	var patterns []string
	for i := range 12 {
		patterns = append(patterns, "["+strings.Repeat("\x00", 4096)+strconv.Itoa(i))
	}
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ac.Spec.AllowedNamespaces = patterns
	ac.Spec.Image.AllowedImages = patterns
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonInvalidNamespacePattern ||
			got.Status.ObservedGeneration != got.Generation {
			return fmt.Errorf("Ready = %+v, want False/%s at generation %d",
				c, kaalmv1beta1.ReasonInvalidNamespacePattern, got.Generation)
		}
		if len(c.Message) >= conditionMessageLimit {
			return fmt.Errorf("Ready message is %d bytes", len(c.Message))
		}
		for _, want := range []string{
			"allowedNamespaces has 2 more malformed entries, not listed",
			"allowedImages has 2 more malformed entries, not listed",
		} {
			if !strings.Contains(c.Message, want) {
				return fmt.Errorf("Ready message does not hold %q", want)
			}
		}
		return nil
	})
}

// longEntries returns twelve distinct entries, each fill repeated 4096 times
// plus its index, after prefix.
func longEntries(prefix, fill string) []string {
	out := make([]string, 12)
	for i := range out {
		out[i] = prefix + strings.Repeat(fill, 4096) + strconv.Itoa(i)
	}
	return out
}

func localRefs(names []string) []kaalmv1beta1.LocalObjectReference {
	out := make([]kaalmv1beta1.LocalObjectReference, len(names))
	for i, n := range names {
		out[i] = kaalmv1beta1.LocalObjectReference{Name: n}
	}
	return out
}

// classReadyAtGeneration waits for the class's Ready condition to be False
// with reason at the current generation, and returns it.
func classReadyAtGeneration(t *testing.T, name, reason string) *metav1.Condition {
	t.Helper()
	var ready *metav1.Condition
	eventually(t, func() error {
		var got kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason ||
			got.Status.ObservedGeneration != got.Generation {
			return fmt.Errorf("Ready = %+v, want False/%s at generation %d", c, reason, got.Generation)
		}
		ready = c
		return nil
	})
	return ready
}

// Many long missing references, CIDRs, and hosts still turn Ready False: each
// list names its first ten entries, quoted up to 256 bytes, and counts the
// rest, so the status write fits the condition message limit.
func TestAgentClass_LongEntryListsTurnReadyFalse(t *testing.T) {
	const name = "ac-long-entries"
	entries := longEntries("", "a")
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ac.Spec.AllowedProviders = localRefs(entries)
	ac.Spec.AllowedToolProviders = localRefs(entries)
	ac.Spec.Network.Egress.AllowedCIDRs = entries
	ac.Spec.Network.Egress.AllowedHosts = entries
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
	c := classReadyAtGeneration(t, name, kaalmv1beta1.ReasonInvalidCIDR)
	if len(c.Message) > conditionMessageLimit {
		t.Fatalf("Ready message is %d bytes", len(c.Message))
	}
	for _, want := range []string{
		"allowedCIDRs has 2 more invalid CIDRs, not listed",
		"allowedHosts has 2 more invalid DNS names, not listed",
		"allowedProviders has 2 more missing ModelProviders, not listed",
		"allowedToolProviders has 2 more missing ToolProviders, not listed",
	} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("Ready message does not hold %q", want)
		}
	}
	if strings.HasSuffix(c.Message, "... (truncated)") {
		t.Errorf("Ready message was cut, want every list whole")
	}
}

// When the bounded lists together still pass the condition message limit
// (control bytes, which the quote writes as four characters each), the
// joined message is cut, and the reason still follows the precedence.
func TestAgentClass_OverlongReadyMessageIsCut(t *testing.T) {
	const name = "ac-overlong-message"
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ac.Spec.Network.Egress.AllowedCIDRs = longEntries("", "\x01")
	ac.Spec.Network.Egress.AllowedHosts = longEntries("", "\x01")
	ac.Spec.AllowedNamespaces = longEntries("[", "\x01")
	ac.Spec.Image.AllowedImages = longEntries("[", "\x01")
	ac.Spec.AllowedProviders = localRefs(longEntries("", "a"))
	ac.Spec.AllowedToolProviders = localRefs(longEntries("", "a"))
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
	c := classReadyAtGeneration(t, name, kaalmv1beta1.ReasonInvalidCIDR)
	if len(c.Message) > conditionMessageLimit {
		t.Fatalf("Ready message is %d bytes", len(c.Message))
	}
	if !strings.HasSuffix(c.Message, "... (truncated)") {
		t.Errorf("Ready message does not end in the cut marker")
	}
}

// Rule 19: invalidCIDRs names each malformed allowedCIDRs entry once, in spec
// order, up to ten, and counts the rest.
func TestInvalidCIDRs(t *testing.T) {
	msg := func(c string) string { return fmt.Sprintf("allowedCIDR %q is not a valid CIDR", c) }
	bad := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "bad" + strconv.Itoa(i)
		}
		return out
	}
	firstTen := func() []string {
		out := make([]string, 10)
		for i, c := range bad(10) {
			out[i] = msg(c)
		}
		return out
	}
	cases := []struct {
		name  string
		cidrs []string
		want  []string
	}{
		{"nil", nil, nil},
		{"valid entries skipped", []string{"10.0.0.0/8", "2001:db8::/32"}, nil},
		{"spec order kept", []string{"b", "10.0.0.0/8", "a"}, []string{msg("b"), msg("a")}},
		{"repeat reported once", []string{"x", "x"}, []string{msg("x")}},
		{"long entry quoted as its first 256 bytes", []string{strings.Repeat("a", 40<<10)},
			[]string{`allowedCIDR "` + strings.Repeat("a", 256) + `" (first 256 of 40960 bytes) is not a valid CIDR`}},
		{"ten entries listed, no count", bad(10), firstTen()},
		{"eleventh entry counted", bad(11), append(firstTen(), "allowedCIDRs has 1 more invalid CIDR, not listed")},
		{"twelfth entry counted", append(bad(12), "bad0"),
			append(firstTen(), "allowedCIDRs has 2 more invalid CIDRs, not listed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &kaalmv1beta1.AgentClass{}
			ac.Spec.Network.Egress.AllowedCIDRs = tc.cidrs
			if got := invalidCIDRs(ac); !slices.Equal(got, tc.want) {
				t.Errorf("invalidCIDRs(%q) = %q, want %q", tc.cidrs, got, tc.want)
			}
		})
	}
}

// Rule 20: invalidHosts names each malformed allowedHosts entry once, in spec
// order, up to ten, and counts the rest.
func TestInvalidHosts(t *testing.T) {
	msg := func(h string) string { return fmt.Sprintf("allowedHost %q is not a valid DNS name", h) }
	bad := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "Bad_" + strconv.Itoa(i)
		}
		return out
	}
	firstTen := func() []string {
		out := make([]string, 10)
		for i, h := range bad(10) {
			out[i] = msg(h)
		}
		return out
	}
	cases := []struct {
		name  string
		hosts []string
		want  []string
	}{
		{"nil", nil, nil},
		{"valid entries skipped", []string{"api.example.com", "example.com"}, nil},
		{"spec order kept", []string{"B_", "ok.example.com", "A_"}, []string{msg("B_"), msg("A_")}},
		{"repeat reported once", []string{"x_", "x_"}, []string{msg("x_")}},
		{"long entry quoted as its first 256 bytes", []string{strings.Repeat("a", 40<<10)},
			[]string{`allowedHost "` + strings.Repeat("a", 256) + `" (first 256 of 40960 bytes) is not a valid DNS name`}},
		{"ten entries listed, no count", bad(10), firstTen()},
		{"eleventh entry counted", bad(11), append(firstTen(), "allowedHosts has 1 more invalid DNS name, not listed")},
		{"twelfth entry counted", append(bad(12), "Bad_0"),
			append(firstTen(), "allowedHosts has 2 more invalid DNS names, not listed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &kaalmv1beta1.AgentClass{}
			ac.Spec.Network.Egress.AllowedHosts = tc.hosts
			if got := invalidHosts(ac); !slices.Equal(got, tc.want) {
				t.Errorf("invalidHosts(%q) = %q, want %q", tc.hosts, got, tc.want)
			}
		})
	}
}

// Many long capabilities.add entries still let the status write through:
// SecurityBaseline lists the first ten, quoted, and counts the rest.
func TestAgentClass_LongCapabilityListFitsStatus(t *testing.T) {
	const name = "ac-long-capabilities"
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	var add []corev1.Capability
	for _, c := range longEntries("", "A") {
		add = append(add, corev1.Capability(c))
	}
	ac.Spec.Security.ContainerSecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: add}}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionSecurityBaseline)
		if c == nil || c.Status != metav1.ConditionFalse || got.Status.ObservedGeneration != got.Generation {
			return fmt.Errorf("SecurityBaseline = %+v, want False at generation %d", c, got.Generation)
		}
		if len(c.Message) > conditionMessageLimit {
			return fmt.Errorf("SecurityBaseline message is %d bytes", len(c.Message))
		}
		if want := "containerSecurityContext.capabilities.add has 2 more capabilities, not listed"; !strings.HasSuffix(c.Message, want) {
			return fmt.Errorf("SecurityBaseline message does not end in %q", want)
		}
		return nil
	})
}
