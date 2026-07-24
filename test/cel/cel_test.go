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

// Package cel exercises the CRD schema validation (CEL and structural) against a
// real apiserver via envtest. Every fixture under test/fixtures/valid must apply;
// every fixture under test/fixtures/invalid must be rejected. This is the
// apply-time half of docs/src/resources/validation-and-defaulting.md.
package cel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

var restCfg *rest.Config

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic("failed to start envtest (run 'make envtest' to fetch binaries): " + err.Error())
	}
	restCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(restCfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func decode(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	u := &unstructured.Unstructured{Object: m}
	if u.GetNamespace() == "" && namespaced(u.GetKind()) {
		u.SetNamespace("default")
	}
	return u
}

func namespaced(kind string) bool {
	switch kind {
	case "Agent", "AgentTask", "AgentChannel":
		return true
	}
	return false
}

func fixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join("..", "..", "test", "fixtures", dir, "*.yaml"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no fixtures in %s (%v)", dir, err)
	}
	return entries
}

// TestValidFixturesApply asserts every valid fixture is accepted by the apiserver.
// A server-side dry-run exercises CEL and structural validation without persisting.
func TestValidFixturesApply(t *testing.T) {
	c := newClient(t)
	for _, f := range fixtures(t, "valid") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.Create(ctx, decode(t, f), client.DryRunAll); err != nil {
				t.Fatalf("expected accept, got: %v", err)
			}
		})
	}
}

// TestInvalidFixturesRejected asserts every invalid fixture is rejected. Each
// fixture is crafted to trip exactly one apply-time rule (see its header comment).
func TestInvalidFixturesRejected(t *testing.T) {
	c := newClient(t)
	for _, f := range fixtures(t, "invalid") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.Create(ctx, decode(t, f), client.DryRunAll); err == nil {
				t.Fatalf("expected rejection, but the apiserver accepted it")
			}
		})
	}
}

// TestClassDerivedDefaultsNotBaked guards the design rule that AgentClass-derived
// values are merged at reconcile time, not by CRD defaulting, so the stored spec
// reflects exactly what the developer wrote (docs/src/resources/validation-and-defaulting.md).
// Intrinsic defaults (activitySource, service.enabled) are fine; class-derived
// fields (image, idleTimeout, persistence.sizeGi) must stay unset.
func TestClassDerivedDefaultsNotBaked(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The parent blocks are present but their class-derived leaves are omitted.
	// Nested CRD defaults only fire when the parent object exists, so this shape
	// is what proves an intrinsic default (activitySource) fills in while a
	// class-derived one (idleTimeout, image, sizeGi) stays unset. (A spec that
	// omits a whole block gets no nested defaults at all, which is why the
	// reconciler must treat an absent block as the documented default in Phase 3.)
	ag := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kaalm.io/v1alpha1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "defaults-probe", "namespace": "default"},
		"spec": map[string]any{
			"agentClassRef": map[string]any{"name": "standard"},
			"lifecycle":     map[string]any{"hibernationEnabled": false},
			"persistence":   map[string]any{"enabled": true},
		},
	}}
	if err := c.Create(ctx, ag); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(ag.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "defaults-probe"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Class-derived fields must be absent from the stored spec.
	classDerived := [][]string{
		{"spec", "image"},
		{"spec", "lifecycle", "idleTimeout"},
		{"spec", "persistence", "sizeGi"},
	}
	for _, path := range classDerived {
		if v, found, _ := unstructured.NestedFieldNoCopy(got.Object, path...); found {
			t.Errorf("%v was baked into the stored spec (%v); class-derived defaults must be reconcile-time only", path, v)
		}
	}
	// Intrinsic default is expected to be present.
	v, found, _ := unstructured.NestedString(got.Object, "spec", "lifecycle", "activitySource")
	if !found || v != "gatewayTraffic" {
		t.Errorf("intrinsic default spec.lifecycle.activitySource = %q, found=%v; want gatewayTraffic", v, found)
	}
}
