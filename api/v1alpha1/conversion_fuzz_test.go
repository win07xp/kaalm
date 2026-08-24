/*
Copyright 2026.

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

package v1alpha1

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metafuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// fuzzIterations is per kind and direction. The whole suite is JSON on
// small objects and stays well under a second.
const fuzzIterations = 200

// fuzzPairs constructs fresh spoke and hub objects per kind.
var fuzzPairs = []struct {
	kind  string
	spoke func() conversion.Convertible
	hub   func() conversion.Hub
}{
	{"AgentClass", func() conversion.Convertible { return &AgentClass{} }, func() conversion.Hub { return &kaalmv1beta1.AgentClass{} }},
	{"ModelProvider", func() conversion.Convertible { return &ModelProvider{} }, func() conversion.Hub { return &kaalmv1beta1.ModelProvider{} }},
	{"ToolProvider", func() conversion.Convertible { return &ToolProvider{} }, func() conversion.Hub { return &kaalmv1beta1.ToolProvider{} }},
	{"Agent", func() conversion.Convertible { return &Agent{} }, func() conversion.Hub { return &kaalmv1beta1.Agent{} }},
	{"AgentTask", func() conversion.Convertible { return &AgentTask{} }, func() conversion.Hub { return &kaalmv1beta1.AgentTask{} }},
	{"AgentChannel", func() conversion.Convertible { return &AgentChannel{} }, func() conversion.Hub { return &kaalmv1beta1.AgentChannel{} }},
}

// TestFuzzRoundTripSpokeHubSpoke proves v1alpha1 to hub and back is the
// identity for every value v1alpha1 can express on the wire; the reverse
// direction below proves the same for v1beta1 and is the drift guard: a
// field added to one version and not the other dies here on the next run.
//
// The contract asserted is wire-form identity, not Go-struct identity: the
// conversion webhook only ever sees JSON, so each fuzzed object is first
// normalized through its own JSON round trip (an empty non-nil slice under
// omitempty and the sub-second part of a timestamp cannot survive any
// apiserver flow), and after the conversion round trip both
// equality.Semantic and the JSON bytes must agree.
func TestFuzzRoundTripSpokeHubSpoke(t *testing.T) {
	fz, seed := newConversionFuzzer(t)
	for _, pair := range fuzzPairs {
		t.Run(pair.kind, func(t *testing.T) {
			for i := 0; i < fuzzIterations; i++ {
				fuzzed := pair.spoke()
				fz.Fuzz(fuzzed)
				fuzzed.GetObjectKind().SetGroupVersionKind(GroupVersion.WithKind(pair.kind))
				src := pair.spoke()
				jsonCopyT(t, fuzzed, src)

				hub := pair.hub()
				if err := src.ConvertTo(hub); err != nil {
					t.Fatalf("seed %d: ConvertTo: %v", seed, err)
				}
				back := pair.spoke()
				if err := back.ConvertFrom(hub); err != nil {
					t.Fatalf("seed %d: ConvertFrom: %v", seed, err)
				}
				requireWireIdentity(t, seed, src, back)
			}
		})
	}
}

// TestFuzzRoundTripHubSpokeHub is the v1beta1-expressible direction.
func TestFuzzRoundTripHubSpokeHub(t *testing.T) {
	fz, seed := newConversionFuzzer(t)
	for _, pair := range fuzzPairs {
		t.Run(pair.kind, func(t *testing.T) {
			for i := 0; i < fuzzIterations; i++ {
				fuzzed := pair.hub()
				fz.Fuzz(fuzzed)
				fuzzed.GetObjectKind().SetGroupVersionKind(kaalmv1beta1.GroupVersion.WithKind(pair.kind))
				src := pair.hub()
				jsonCopyT(t, fuzzed, src)

				spoke := pair.spoke()
				if err := spoke.ConvertFrom(src); err != nil {
					t.Fatalf("seed %d: ConvertFrom: %v", seed, err)
				}
				back := pair.hub()
				if err := spoke.ConvertTo(back); err != nil {
					t.Fatalf("seed %d: ConvertTo: %v", seed, err)
				}
				requireWireIdentity(t, seed, src, back)
			}
		})
	}
}

// newConversionFuzzer builds the apimachinery fuzzer with the meta funcs
// (valid ObjectMeta, Time, ManagedFields, resource.Quantity) over both
// registered versions. The seed is random and logged so a failure can be
// replayed by hardcoding it.
func newConversionFuzzer(t *testing.T) (fz interface{ Fuzz(any) }, seed int64) {
	t.Helper()
	seed = time.Now().UnixNano()
	t.Logf("fuzz seed %d", seed)
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fuzzer.FuzzerFor(metafuzzer.Funcs, rand.NewSource(seed), runtimeserializer.NewCodecFactory(scheme)), seed
}

// jsonCopyT normalizes src into dst through the wire form.
func jsonCopyT(t *testing.T, src, dst runtime.Object) {
	t.Helper()
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal %T: %v", src, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("unmarshal into %T: %v", dst, err)
	}
}

// requireWireIdentity asserts the two objects are identical both as Go
// values (semantic equality) and as JSON bytes.
func requireWireIdentity(t *testing.T, seed int64, want, got runtime.Object) {
	t.Helper()
	if !apiequality.Semantic.DeepEqual(want, got) {
		t.Fatalf("seed %d: conversion round trip lost data:\n%s", seed, cmp.Diff(mustJSON(t, want), mustJSON(t, got)))
	}
	if w, g := mustJSON(t, want), mustJSON(t, got); w != g {
		t.Fatalf("seed %d: wire forms differ:\n%s", seed, cmp.Diff(w, g))
	}
}

func mustJSON(t *testing.T, obj runtime.Object) string {
	t.Helper()
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatalf("marshal %T: %v", obj, err)
	}
	return string(raw)
}
