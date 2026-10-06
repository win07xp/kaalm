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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// deletingProvider is a ModelProvider under deletion that still carries its
// finalizer.
func deletingProvider(name string) *kaalmv1beta1.ModelProvider {
	return eventsProvider(name, func(mp *kaalmv1beta1.ModelProvider) {
		now := metav1.Now()
		mp.DeletionTimestamp = &now
		mp.UID = "uid-1"
	})
}

// spendConfigMaps returns the provider's budget and agent-spend ConfigMaps.
func spendConfigMaps(name string) []client.Object {
	return []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: testOperatorNamespace, Name: gateway.BudgetConfigMapName(name),
		}, Data: map[string]string{"gw-0": `{"period":"2026-10","team-a":"95.00"}`}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: testOperatorNamespace, Name: gateway.AgentSpendConfigMapName(name),
		}, Data: map[string]string{"gw-0": `{"period":"2026-10","team-a/agent/x":"95.00"}`}},
	}
}

// deleteReconciler builds a ModelProviderReconciler over a fake client with
// the three referrer indexes registered.
func deleteReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *ModelProviderReconciler {
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
	byAllowed := func(o client.Object) []string {
		ac := o.(*kaalmv1beta1.AgentClass)
		out := make([]string, 0, len(ac.Spec.AllowedProviders))
		for _, p := range ac.Spec.AllowedProviders {
			out = append(out, p.Name)
		}
		return out
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.ModelProvider{}).
		WithIndex(&kaalmv1beta1.Agent{}, IndexProviderRef, byProviderRef).
		WithIndex(&kaalmv1beta1.AgentTask{}, IndexProviderRef, byProviderRef).
		WithIndex(&kaalmv1beta1.AgentClass{}, IndexAllowedProviders, byAllowed).
		WithInterceptorFuncs(funcs).
		Build()
	return &ModelProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(32),
		OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
	}
}

func configMapExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testOperatorNamespace, Name: name}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get ConfigMap %s: %v", name, err)
	}
	return err == nil
}

// A released delete removes the provider's spend ConfigMaps, so a provider
// recreated under the same name starts from zero spend.
func TestModelProvider_DeleteRemovesSpendConfigMaps(t *testing.T) {
	name := "del-cms"
	r := deleteReconciler(t, interceptor.Funcs{}, append(spendConfigMaps(name), deletingProvider(name))...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, cm := range []string{gateway.BudgetConfigMapName(name), gateway.AgentSpendConfigMapName(name)} {
		if configMapExists(t, r.Client, cm) {
			t.Errorf("ConfigMap %s still exists after the delete was released", cm)
		}
	}
	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &mp); !apierrors.IsNotFound(err) {
		t.Errorf("provider still present (err=%v, finalizers=%v)", err, mp.Finalizers)
	}
}

// A ConfigMap delete that fails keeps the finalizer, so the next pass
// retries before the name can be reused.
func TestModelProvider_DeleteKeepsFinalizerWhenConfigMapDeleteFails(t *testing.T) {
	name := "del-cm-fail"
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			return apierrors.NewInternalError(context.DeadlineExceeded)
		}
		return c.Delete(ctx, obj, opts...)
	}}
	r := deleteReconciler(t, funcs, append(spendConfigMaps(name), deletingProvider(name))...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err == nil {
		t.Fatal("reconcile returned nil; want the ConfigMap delete error")
	}
	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &mp); err != nil {
		t.Fatalf("provider gone after a failed ConfigMap delete: %v", err)
	}
	if len(mp.Finalizers) == 0 {
		t.Error("finalizer released although a ConfigMap delete failed")
	}
}

// While referrers hold the delete, the gateway keeps counting their spend,
// so the ConfigMaps stay.
func TestModelProvider_HeldDeleteKeepsSpendConfigMaps(t *testing.T) {
	name := "del-held"
	class := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "holds-" + name},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedProviders: []kaalmv1beta1.LocalObjectReference{{Name: name}},
		},
	}
	r := deleteReconciler(t, interceptor.Funcs{}, append(spendConfigMaps(name), deletingProvider(name), class)...)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &mp); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if c := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil || c.Reason != kaalmv1beta1.ReasonDeletionBlocked {
		t.Errorf("Ready = %+v, want reason DeletionBlocked", c)
	}
	for _, cm := range []string{gateway.BudgetConfigMapName(name), gateway.AgentSpendConfigMapName(name)} {
		if !configMapExists(t, r.Client, cm) {
			t.Errorf("ConfigMap %s deleted while the delete is held", cm)
		}
	}
}

// A released delete drops the provider's canonical-spend series; a held
// delete keeps them; a provider gone before the delete pass loses them on
// the next request for its name.
func TestModelProvider_DeleteDropsCanonicalGauge(t *testing.T) {
	period := gateway.PeriodKey("monthly", time.Now())
	cases := []struct {
		name     string
		objs     func(name string) []client.Object
		wantKept bool
	}{{
		name: "finalizer released",
		objs: func(name string) []client.Object { return []client.Object{deletingProvider(name)} },
	}, {
		name: "held by referrer",
		objs: func(name string) []client.Object {
			return []client.Object{deletingProvider(name), &kaalmv1beta1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "holds", Namespace: "team-a"},
				Spec: kaalmv1beta1.AgentSpec{
					Providers: []kaalmv1beta1.AgentProviderReference{{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: name}}},
				},
			}}
		},
		wantKept: true,
	}, {
		name: "object already gone",
		objs: func(string) []client.Object { return nil },
	}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("del-gauge-%d", i)
			control := name + "-control"
			t.Cleanup(func() {
				providerBudgetCanonical.DeletePartialMatch(prometheus.Labels{"provider": name})
				providerBudgetCanonical.DeletePartialMatch(prometheus.Labels{"provider": control})
			})
			providerBudgetCanonical.WithLabelValues(name, "team-a", period).Set(95)
			providerBudgetCanonical.WithLabelValues(control, "team-a", period).Set(1)
			r := deleteReconciler(t, interceptor.Funcs{}, tc.objs(name)...)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := len(budgetCanonicalSeries(name)); (got == 1) != tc.wantKept {
				t.Errorf("%s series = %d, want kept=%v", name, got, tc.wantKept)
			}
			if got := len(budgetCanonicalSeries(control)); got != 1 {
				t.Errorf("control provider series = %d, want 1", got)
			}
		})
	}
}
