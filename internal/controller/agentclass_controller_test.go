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
