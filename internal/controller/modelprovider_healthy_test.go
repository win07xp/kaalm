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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// expectNotProbed checks that Healthy is Unknown with reason NotProbed and a
// message containing why.
func expectNotProbed(t *testing.T, conds []metav1.Condition, why string) {
	t.Helper()
	c := condition(conds, kaalmv1beta1.ConditionHealthy)
	if c == nil {
		t.Fatalf("Healthy is absent, want Unknown/%s", kaalmv1beta1.ReasonNotProbed)
	}
	if c.Status != metav1.ConditionUnknown || c.Reason != kaalmv1beta1.ReasonNotProbed {
		t.Fatalf("Healthy = %s/%s, want Unknown/%s", c.Status, c.Reason, kaalmv1beta1.ReasonNotProbed)
	}
	if !strings.Contains(c.Message, why) {
		t.Fatalf("Healthy message %q does not contain %q", c.Message, why)
	}
}

// A ModelProvider pass that ends without probing must not keep a Healthy
// value from an earlier probe: it sets Healthy=Unknown with NotProbed.
func TestModelProvider_HealthyNotProbedWhenPassEndsEarly(t *testing.T) {
	cases := []struct {
		name      string
		objects   func(name string) []client.Object
		mutate    func(mp *kaalmv1beta1.ModelProvider)
		ready     metav1.ConditionStatus
		readyWhy  string
		healthWhy string
	}{
		{
			name:     "credentials missing",
			objects:  func(string) []client.Object { return nil },
			ready:    metav1.ConditionFalse,
			readyWhy: kaalmv1beta1.ReasonCredentialsMissing,
		},
		{
			name: "secret not opted in",
			objects: func(name string) []client.Object {
				return []client.Object{&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: name + "-key", Namespace: testOperatorNamespace,
						Annotations: map[string]string{kaalmv1beta1.AnnotationProviderHosts: testProviderHosts},
					},
					Data: map[string][]byte{"token": []byte("sk-test")},
				}}
			},
			ready:    metav1.ConditionFalse,
			readyWhy: kaalmv1beta1.ReasonSecretNotOptedIn,
		},
		{
			name: "endpoint host not approved",
			objects: func(name string) []client.Object {
				return []client.Object{&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: name + "-key", Namespace: testOperatorNamespace,
						Labels:      map[string]string{kaalmv1beta1.LabelProviderCredential: kaalmv1beta1.AnnotationTrue},
						Annotations: map[string]string{kaalmv1beta1.AnnotationProviderHosts: "other.example.com"},
					},
					Data: map[string][]byte{"token": []byte("sk-test")},
				}}
			},
			ready:    metav1.ConditionFalse,
			readyWhy: kaalmv1beta1.ReasonEndpointHostNotApproved,
		},
		{
			name:    "configuration check fails",
			objects: func(name string) []client.Object { return []client.Object{providerKey(name)} },
			mutate: func(mp *kaalmv1beta1.ModelProvider) {
				mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "no-such-provider"}}
			},
			ready:    metav1.ConditionFalse,
			readyWhy: kaalmv1beta1.ReasonFallbackIneligible,
		},
		{
			name:    "probe disabled",
			objects: func(name string) []client.Object { return []client.Object{providerKey(name)} },
			mutate: func(mp *kaalmv1beta1.ModelProvider) {
				mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false}
			},
			ready:     metav1.ConditionTrue,
			readyWhy:  kaalmv1beta1.ReasonCredentialsValid,
			healthWhy: "healthCheck.enabled is false",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			name := "mp-np-" + string(rune('a'+i))
			mp := probedProvider(name, metav1.ConditionTrue, time.Now().Add(-time.Hour))
			if tc.mutate != nil {
				tc.mutate(mp)
			}
			objs := append([]client.Object{mp}, tc.objects(name)...)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(objs...).WithStatusSubresource(mp).Build()
			health := newFakeHealth()
			r := &ModelProviderReconciler{
				Client: c, Recorder: record.NewFakeRecorder(10),
				OperatorNamespace: testOperatorNamespace, Health: health,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var got kaalmv1beta1.ModelProvider
			if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			ready := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if ready == nil || ready.Status != tc.ready || ready.Reason != tc.readyWhy {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tc.ready, tc.readyWhy)
			}
			why := tc.healthWhy
			if why == "" {
				why = tc.readyWhy
			}
			expectNotProbed(t, got.Status.Conditions, why)
			if n := health.count(name); n != 0 {
				t.Fatalf("probe ran %d times on a pass that should not probe", n)
			}
		})
	}
}

// A repeated NotProbed pass derives the same condition, so it makes no
// status write.
func TestModelProvider_NotProbedPassIsStable(t *testing.T) {
	ctx := context.Background()
	mp := probedProvider("mp-np-stable", metav1.ConditionTrue, time.Now().Add(-time.Hour))
	writes := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(mp).WithStatusSubresource(mp).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ModelProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(10),
		OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "mp-np-stable"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var settled kaalmv1beta1.ModelProvider
	if err := c.Get(ctx, req.NamespacedName, &settled); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("first pass made %d status writes, want 1", writes)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var after kaalmv1beta1.ModelProvider
	if err := c.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || after.ResourceVersion != settled.ResourceVersion {
		t.Errorf("a repeated NotProbed pass rewrote status: writes=%d, resourceVersion %s -> %s",
			writes, settled.ResourceVersion, after.ResourceVersion)
	}
}
