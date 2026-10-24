//go:build secretbench

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

package main

import (
	"strings"
	"testing"
)

func TestNamespaceForLayouts(t *testing.T) {
	if got := namespaceFor(7, layoutOne, 20); got != oneNamespace {
		t.Fatalf("one layout: %q, want %q", got, oneNamespace)
	}
	cases := map[int]string{0: "secretbench-00", 3: "secretbench-03", 20: "secretbench-00", 23: "secretbench-03"}
	for i, want := range cases {
		if got := namespaceFor(i, layoutSpread, 20); got != want {
			t.Fatalf("spread layout, Secret %d: %q, want %q", i, got, want)
		}
	}
}

func TestNamespacesUsedCapsAtCount(t *testing.T) {
	cases := []struct {
		count, namespaces int
		layout            string
		want              int
	}{
		{0, 20, layoutSpread, 0},
		{5, 20, layoutSpread, 5},
		{250, 20, layoutSpread, 20},
		{0, 20, layoutOne, 0},
		{250, 20, layoutOne, 1},
	}
	for _, c := range cases {
		if got := namespacesUsed(c.count, c.layout, c.namespaces); got != c.want {
			t.Errorf("namespacesUsed(%d, %s, %d) = %d, want %d", c.count, c.layout, c.namespaces, got, c.want)
		}
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Counts; len(got) != 6 || got[0] != 0 || got[5] != 5000 {
		t.Fatalf("counts = %v, want 0..5000", got)
	}
	if cfg.Repeats != 3 || cfg.Namespaces != 20 || len(cfg.Variants) != 7 || len(cfg.Layouts) != 2 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.Variants[5] != "lean" {
		t.Fatalf("variants = %v, want lean after C-ctrl", cfg.Variants)
	}
	if !strings.HasPrefix(cfg.Out, "test/secretbench/results/") {
		t.Fatalf("out = %q, want it under test/secretbench/results/", cfg.Out)
	}
}

func TestParseFlagsRejectsUnorderedCounts(t *testing.T) {
	for _, counts := range []string{"0,500,250", "0,250,250", "-1,5", "x"} {
		if _, err := parseFlags([]string{"-counts", counts}); err == nil {
			t.Errorf("-counts %s accepted", counts)
		}
	}
}

func TestParseFlagsRejectsUnknownVariant(t *testing.T) {
	if _, err := parseFlags([]string{"-variants", "today-gw,D"}); err == nil {
		t.Fatal("unknown variant accepted")
	}
	if _, err := parseFlags([]string{"-layouts", "many"}); err == nil {
		t.Fatal("unknown layout accepted")
	}
	cfg, err := parseFlags([]string{"-variants", "A,today-ctrl"})
	if err != nil {
		t.Fatal(err)
	}
	got := selectVariants(cfg)
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "today-ctrl" {
		t.Fatalf("selectVariants = %v, want A then today-ctrl", got)
	}
}
