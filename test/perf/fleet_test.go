//go:build perftest

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
	"context"
	"errors"
	"testing"
	"time"
)

func TestFleetNamesAndPlacement(t *testing.T) {
	f := fleet{phase: "p", prefix: "spread-", target: 5, namespaces: []string{"a", "b", "c"}}
	if got := f.name(7); got != "spread-0007" {
		t.Errorf("name(7) = %q", got)
	}
	want := []string{"a", "b", "c", "a", "b"}
	for i, ns := range want {
		if got := f.namespaceOf(i); got != ns {
			t.Errorf("namespaceOf(%d) = %q, want %q", i, got, ns)
		}
	}
	if f.scope() != "" {
		t.Errorf("a spread fleet lists every namespace, got scope %q", f.scope())
	}
	one := fleet{namespaces: []string{"perf"}}
	if one.scope() != "perf" || one.namespaceOf(42) != "perf" {
		t.Errorf("a one-namespace fleet stays in it: scope %q, namespaceOf %q", one.scope(), one.namespaceOf(42))
	}
}

func TestSpreadNamespaces(t *testing.T) {
	got := spreadNamespaces("perf", 20)
	if len(got) != 20 || got[0] != "perf-00" || got[9] != "perf-09" || got[19] != "perf-19" {
		t.Errorf("spreadNamespaces(perf, 20) = %v", got)
	}
}

func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("canceled context: waited %s, want an early return", took)
	}

	start = time.Now()
	if err := sleepCtx(context.Background(), 10*time.Millisecond); err != nil {
		t.Errorf("background context: err = %v", err)
	}
	if took := time.Since(start); took < 10*time.Millisecond {
		t.Errorf("background context: returned after %s, want the full 10ms", took)
	}
}
