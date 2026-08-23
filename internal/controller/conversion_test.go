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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// TestConversion_ServedThroughTheAPIServer proves the wiring the suite relies
// on for every other test: the CRDs envtest installed carry a webhook
// conversion pointed at this process, an object written at v1alpha1 is
// stored at v1beta1 and reads back at either version with the same spec, and
// a write at v1beta1 is visible at v1alpha1. This is the in-process twin of
// the cluster path the controller serves on its conversion port.
func TestConversion_ServedThroughTheAPIServer(t *testing.T) {
	ctx := context.Background()

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	})
	if err := testClient.Get(ctx, types.NamespacedName{Name: "agents.kaalm.io"}, crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	strategy, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "strategy")
	if strategy != "Webhook" {
		t.Fatalf("agents.kaalm.io conversion strategy = %q, want Webhook (envtest did not install the conversion webhook)", strategy)
	}
	stored, _, _ := unstructured.NestedStringSlice(crd.Object, "status", "storedVersions")
	if len(stored) != 1 || stored[0] != "v1beta1" {
		t.Fatalf("agents.kaalm.io storedVersions = %v, want [v1beta1]", stored)
	}

	alpha := &kaalmv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "conv-agent", Namespace: "default"},
		Spec: kaalmv1alpha1.AgentSpec{
			AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: "conv-class"},
			Image:         "example/agent:one",
			Lifecycle: kaalmv1alpha1.AgentLifecycle{
				IdleTimeout:    metav1.Duration{Duration: 90 * time.Second},
				ActivitySource: "both",
			},
			Persistence: kaalmv1alpha1.AgentPersistence{Enabled: true, MountPath: "/data"},
		},
	}
	if err := testClient.Create(ctx, alpha); err != nil {
		t.Fatalf("create v1alpha1 agent: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), alpha) })

	beta := &kaalmv1beta1.Agent{}
	key := types.NamespacedName{Name: "conv-agent", Namespace: "default"}
	eventually(t, func() error { return testClient.Get(ctx, key, beta) })
	alphaSpec, _ := json.Marshal(alpha.Spec)
	betaSpec, _ := json.Marshal(beta.Spec)
	if string(alphaSpec) != string(betaSpec) {
		t.Fatalf("spec differs across versions\nv1alpha1: %s\nv1beta1:  %s", alphaSpec, betaSpec)
	}

	// A write at the hub version is visible at the spoke version.
	beta.Spec.Image = "example/agent:two"
	if err := testClient.Update(ctx, beta); err != nil {
		t.Fatalf("update v1beta1 agent: %v", err)
	}
	eventually(t, func() error {
		got := &kaalmv1alpha1.Agent{}
		if err := testClient.Get(ctx, key, got); err != nil {
			return err
		}
		if got.Spec.Image != "example/agent:two" {
			return fmt.Errorf("v1alpha1 image = %q, want example/agent:two", got.Spec.Image)
		}
		return nil
	})
}
