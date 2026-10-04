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
// real apiserver via envtest. Every fixture under test/fixtures/valid and every
// manifest under config/samples must apply with strict field validation; every
// fixture under test/fixtures/invalid must be rejected by the one rule its
// `# expect:` line names. This is the apply-time
// half of docs/src/resources/validation-and-defaulting.md.
package cel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/win07xp/kaalm/internal/testenv"
)

var restCfg *rest.Config

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	// testenv.Start stops the control plane itself when the start fails.
	cfg, err := testenv.Start(env)
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

// dryRunCreate dry-runs the create of u against the CRD schema and CEL rules
// and rejects any field the schema does not know. Default field validation only
// prunes such a field with a warning, so a stale or misspelled field would pass.
func dryRunCreate(ctx context.Context, c client.Client, u *unstructured.Unstructured) error {
	return c.Create(ctx, u, client.DryRunAll, client.FieldValidation("Strict"))
}

// TestDryRunCreateRejectsUnknownField guards dryRunCreate: an object with a
// misspelled field must be rejected, not accepted with the field pruned.
func TestDryRunCreateRejectsUnknownField(t *testing.T) {
	c := newClient(t)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kaalm.io/v1beta1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "unknown-field-probe", "namespace": "default"},
		"spec": map[string]any{
			"agentClassRef": map[string]any{"name": "standard"},
			"imagee":        "ghcr.io/win07xp/agent:latest",
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := dryRunCreate(ctx, c, u)
	if err == nil {
		t.Fatal("the apiserver accepted an unknown field; the dry-run must use strict field validation")
	}
	if !strings.Contains(err.Error(), `unknown field "spec.imagee"`) {
		t.Fatalf("expected an unknown field error for spec.imagee, got: %v", err)
	}
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

// TestValidFixturesApply asserts every valid fixture is accepted by the apiserver
// with strict field validation, so a stale or misspelled field fails the test
// instead of being pruned with a warning. A server-side dry-run exercises CEL and
// structural validation without persisting.
func TestValidFixturesApply(t *testing.T) {
	c := newClient(t)
	for _, f := range fixtures(t, "valid") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := dryRunCreate(ctx, c, decode(t, f)); err != nil {
				t.Fatalf("expected accept, got: %v", err)
			}
		})
	}
}

// decodeDocs splits a multi-document YAML file into one object per document.
// Empty documents are skipped. A document without apiVersion or kind fails the
// test, because a file in config/samples must hold resources only.
func decodeDocs(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var docs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for i := 0; ; i++ {
		m := map[string]any{}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s document %d: %v", path, i+1, err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			t.Fatalf("%s document %d: missing apiVersion or kind; not a resource", path, i+1)
		}
		if u.GetNamespace() == "" && namespaced(u.GetKind()) {
			u.SetNamespace("default")
		}
		docs = append(docs, u)
	}
	if len(docs) == 0 {
		t.Fatalf("%s: no resources", path)
	}
	return docs
}

// TestSamplesApply asserts every manifest in config/samples, including the
// ones not listed in the samples kustomization, passes the CRD schema and CEL
// rules and has no field the schema does not know. Strict field validation
// rejects unknown fields, which the default dry-run only prunes with a warning.
func TestSamplesApply(t *testing.T) {
	c := newClient(t)
	all, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "*.yaml"))
	if err != nil {
		t.Fatalf("glob samples: %v", err)
	}
	var files []string
	for _, f := range all {
		// kustomization.yaml is a kustomize config, not a resource.
		if filepath.Base(f) != "kustomization.yaml" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatalf("no samples in config/samples")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			for i, u := range decodeDocs(t, f) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := dryRunCreate(ctx, c, u)
				cancel()
				if err != nil {
					t.Errorf("document %d (%s %s): expected accept, got: %v", i+1, u.GetKind(), u.GetName(), err)
				}
			}
		})
	}
}

// expectedRejection returns the fragment of the rejection message an invalid
// fixture declares on its one `# expect:` line.
func expectedRejection(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "# expect:"); ok {
			found = append(found, strings.TrimSpace(rest))
		}
	}
	if len(found) != 1 || found[0] == "" {
		t.Fatalf("%s: every invalid fixture needs exactly one non-empty `# expect:` line "+
			"with part of the rejection message, found %q", path, found)
	}
	return found[0]
}

// TestInvalidFixturesRejected asserts every invalid fixture is rejected for the
// reason it was written for. Each fixture is crafted to trip exactly one
// apply-time rule, named in its header comment, and its `# expect:` line holds
// part of that rule's message. The test fails when the fixture is rejected for
// another reason, or by more than one rule.
func TestInvalidFixturesRejected(t *testing.T) {
	c := newClient(t)
	for _, f := range fixtures(t, "invalid") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			want := expectedRejection(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := dryRunCreate(ctx, c, decode(t, f))
			if err == nil {
				t.Fatalf("expected rejection, but the apiserver accepted it")
			}
			var status *apierrors.StatusError
			if !errors.As(err, &status) || status.ErrStatus.Details == nil || len(status.ErrStatus.Details.Causes) != 1 {
				t.Fatalf("expected exactly one rule violation, got: %v", err)
			}
			if !strings.Contains(status.ErrStatus.Details.Causes[0].Message, want) {
				t.Fatalf("rejected for the wrong reason: want a message containing %q, got: %v", want, err)
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
		"apiVersion": "kaalm.io/v1beta1",
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
