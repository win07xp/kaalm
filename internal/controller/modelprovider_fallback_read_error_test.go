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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A fallback read that fails with anything but NotFound fails the pass: the
// error goes back to controller-runtime for a backoff retry, and the
// provider is never reported valid with an unchecked fallback edge.
func TestModelProvider_FallbackReadErrorFailsThePass(t *testing.T) {
	const name = "primary"
	errInjected := errors.New("injected fallback read failure")
	primary := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ProviderFinalizer},
		},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:              "openai",
			Endpoint:          "https://api.example.com",
			CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
			AllowedNamespaces: []string{"*"},
			HealthCheck:       &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false},
			Fallback:          []kaalmv1beta1.FallbackReference{{Name: "fb"}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: providerCredentialMeta(name+"-key", testOperatorNamespace),
		Data:       map[string][]byte{"token": []byte("sk-test")},
	}
	r := deleteReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*kaalmv1beta1.ModelProvider); ok && key.Name == "fb" {
				return errInjected
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, primary, secret)

	_, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reconcile returned %v, want the injected read error", err)
	}
	var got kaalmv1beta1.ModelProvider
	if err := r.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	if apimeta.IsStatusConditionTrue(got.Status.Conditions, kaalmv1beta1.ConditionReady) {
		t.Errorf("a failed fallback read stored Ready=True: %+v",
			apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady))
	}
}
