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

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Counts; len(got) != 6 || got[0] != 0 || got[5] != 5000 {
		t.Fatalf("counts = %v, want 0..5000", got)
	}
	if cfg.Repeats != 3 || cfg.Namespaces != 20 || len(cfg.Layouts) != 2 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.QPS != gatewayQPS || cfg.Burst != gatewayBurst {
		t.Fatalf("client limit = QPS %v, burst %d; want the gateway's %d and %d",
			cfg.QPS, cfg.Burst, gatewayQPS, gatewayBurst)
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

func TestParseFlagsRejectsUnknownLayout(t *testing.T) {
	if _, err := parseFlags([]string{"-layouts", "many"}); err == nil {
		t.Fatal("unknown layout accepted")
	}
}

func TestParseFlagsClientLimit(t *testing.T) {
	cfg, err := parseFlags([]string{"-qps", "20", "-burst", "30"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QPS != 20 || cfg.Burst != 30 {
		t.Fatalf("client limit = QPS %v, burst %d; want 20 and 30", cfg.QPS, cfg.Burst)
	}
	for _, args := range [][]string{{"-qps", "0"}, {"-qps", "-5"}, {"-burst", "0"}, {"-burst", "-1"}} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestParseFlagsRBACCheck(t *testing.T) {
	cfg, err := parseFlags([]string{"-rbac-check"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RBACCheck || cfg.Out != "" {
		t.Fatalf("cfg = %+v, want RBACCheck and no summary path", cfg)
	}
}
