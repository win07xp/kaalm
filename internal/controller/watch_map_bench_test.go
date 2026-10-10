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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// benchWatchObjects is how many ModelProviders, ToolProviders, and
// AgentClasses the watch map function benchmarks list.
const benchWatchObjects = 50

var benchWatchOnce sync.Once

// seedWatchBench creates the providers and classes the map function
// benchmarks list, once per test binary, and waits until the manager's
// cache holds them. Every provider declares a fallback and names the same
// credential Secret, and every class allows the first provider of each
// kind, so each map function returns all of them.
func seedWatchBench(b *testing.B) {
	b.Helper()
	benchWatchOnce.Do(func() {
		ctx := context.Background()
		for i := range benchWatchObjects {
			objs := []client.Object{
				&kaalmv1beta1.ModelProvider{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bench-mp-%d", i)},
					Spec: kaalmv1beta1.ModelProviderSpec{
						Type: "openai", Endpoint: "https://api.example.com",
						CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: "bench-key", Key: "token"},
						AllowedNamespaces: []string{"*"},
						Fallback:          []kaalmv1beta1.FallbackReference{{Name: "bench-mp-0"}},
						Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}, {ID: "m2"}},
					},
				},
				&kaalmv1beta1.ToolProvider{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bench-tp-%d", i)},
					Spec: kaalmv1beta1.ToolProviderSpec{
						Type: "mcp", Endpoint: "https://mcp.example.com",
						CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: "bench-key", Key: "token"},
					},
				},
				&kaalmv1beta1.AgentClass{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bench-class-%d", i)},
					Spec: kaalmv1beta1.AgentClassSpec{
						AllowedProviders:     []kaalmv1beta1.LocalObjectReference{{Name: "bench-mp-0"}},
						AllowedToolProviders: []kaalmv1beta1.LocalObjectReference{{Name: "bench-tp-0"}},
					},
				},
			}
			for _, o := range objs {
				if err := testClient.Create(ctx, o); err != nil {
					b.Fatalf("create %s: %v", o.GetName(), err)
				}
			}
		}
		deadline := time.Now().Add(timeout)
		for {
			var mps kaalmv1beta1.ModelProviderList
			var tps kaalmv1beta1.ToolProviderList
			var acs kaalmv1beta1.AgentClassList
			if testCache.List(ctx, &mps) == nil && testCache.List(ctx, &tps) == nil &&
				testCache.List(ctx, &acs) == nil && len(mps.Items) >= benchWatchObjects &&
				len(tps.Items) >= benchWatchObjects && len(acs.Items) >= benchWatchObjects {
				return
			}
			if time.Now().After(deadline) {
				b.Fatal("the cache did not see the benchmark objects")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// The watch map functions run on every event of the watched kind and only
// read names and spec fields from the cache, so they list without a deep
// copy.
func BenchmarkWatchMapFuncs(b *testing.B) {
	seedWatchBench(b)
	mp := &ModelProviderReconciler{Client: testClient, OperatorNamespace: testOperatorNamespace}
	tp := &ToolProviderReconciler{Client: testClient, OperatorNamespace: testOperatorNamespace}
	ac := &AgentClassReconciler{Client: testClient}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bench-key", Namespace: testOperatorNamespace}}
	provider := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "bench-mp-0"}}
	tool := &kaalmv1beta1.ToolProvider{ObjectMeta: metav1.ObjectMeta{Name: "bench-tp-0"}}
	other := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "bench-unrelated"}}
	cases := []struct {
		name string
		fn   func(context.Context, client.Object) []reconcile.Request
		obj  client.Object
	}{
		{"providersWithFallback", mp.providersWithFallback, other},
		{"providersForSecret", mp.providersForSecret, secret},
		{"allModelProviders", mp.allModelProviders, nil},
		{"toolProvidersForSecret", tp.toolProvidersForSecret, secret},
		{"allClasses", ac.allClasses, nil},
		{"classesForProvider", ac.classesForProvider, provider},
		{"classesForToolProvider", ac.classesForToolProvider, tool},
	}
	ctx := context.Background()
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			if n := len(tc.fn(ctx, tc.obj)); n < benchWatchObjects {
				b.Fatalf("%s returned %d requests, want at least %d", tc.name, n, benchWatchObjects)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				tc.fn(ctx, tc.obj)
			}
		})
	}
}
