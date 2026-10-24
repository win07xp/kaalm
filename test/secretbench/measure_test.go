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
	"context"
	"os"
	"testing"
	"time"
)

// heldForRSS keeps an allocation reachable until the test drops it, so the
// compiler cannot free it early.
var heldForRSS []byte

// A GC frees heap spans but keeps their pages until the background scavenger
// returns them, so an RSS read right after a GC still holds what the previous
// variant dropped. releaseMemory must hand those pages back first.
func TestReleaseMemoryReturnsFreedPagesToTheOS(t *testing.T) {
	const size = 256 << 20
	heldForRSS = make([]byte, size)
	for i := range heldForRSS {
		heldForRSS[i] = 1
	}
	held, err := rssBytes(os.Getpid())
	if err != nil {
		t.Skipf("no /proc RSS: %v", err)
	}
	heldForRSS = nil
	releaseMemory()
	after, err := rssBytes(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if held-after < size/2 {
		t.Fatalf("RSS %d MiB while holding, %d MiB after releaseMemory; want at least %d MiB returned",
			held>>20, after>>20, size>>21)
	}
}

func TestMeasureVariantSamplesOneStep(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	for _, name := range []string{"s0", "s1", "s2", "s3", "s4"} {
		createSecret(t, ns, name, true)
	}
	names := secretKeysIn(ns, "s0", "s1", "s2", "s3", "s4")

	// Earlier tests' watches close on the server a moment after their
	// clients stop, so the baseline waits for the gauge to stop moving.
	var baseline float64
	eventually(t, 10*time.Second, "a steady Secret watch count", func() bool {
		a, errA := apiServerSecretWatches(ctx, adminClient)
		time.Sleep(300 * time.Millisecond)
		b, errB := apiServerSecretWatches(ctx, adminClient)
		baseline = b
		return errA == nil && errB == nil && a == b
	})
	pids, _ := findControlPlane()
	cfg := config{Readers: 4, UpdateSamples: 2, SettleTimeout: 30 * time.Second}
	b := &bench{base: restCfg, admin: adminClient, pids: pids, baselineWatches: baseline, cfg: cfg}

	var a variant
	for _, v := range variants(config{BQPS: 1, BBurst: 1}) {
		if v.Name == "A" {
			a = v
		}
	}
	s, err := b.measure(ctx, a, layoutOne, 1, names, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Secrets != 5 || s.ServedSec <= 0 || !s.Settled || s.ReadErrors != 0 {
		t.Fatalf("sample = %+v, want 5 Secrets served and settled without errors", s)
	}
	if s.Requests.List != 1 || s.Requests.Watch != 1 || s.Requests.Get != 0 {
		t.Fatalf("requests = %+v, want List 1, Watch 1, Get 0", s.Requests)
	}
	if s.RequestsDuringUpdates != (requestCounts{}) {
		t.Fatalf("requests during updates = %+v, want none", s.RequestsDuringUpdates)
	}
	if s.UpdateMissed != 0 || s.UpdateP50Ms <= 0 {
		t.Fatalf("updates: missed %d, p50 %v ms; want none missed and a positive p50", s.UpdateMissed, s.UpdateP50Ms)
	}
	if s.APIServerSecretWatches != 1 {
		t.Fatalf("API server Secret watches = %v, want 1", s.APIServerSecretWatches)
	}
	if s.HeapInuseBytes <= 0 || s.Goroutines <= 0 {
		t.Fatalf("heap %d, goroutines %d; want both positive", s.HeapInuseBytes, s.Goroutines)
	}
	if pids.APIServer != 0 && s.APIServerRSSBytes <= 0 {
		t.Fatalf("kube-apiserver RSS = %d, want positive", s.APIServerRSSBytes)
	}
	if len(s.Notes) != 0 {
		t.Fatalf("notes = %v, want none (drained after Stop)", s.Notes)
	}
	watches, err := apiServerSecretWatches(ctx, adminClient)
	if err != nil || watches > baseline {
		t.Fatalf("API server Secret watches after Stop = %v (%v), want at most %v", watches, err, baseline)
	}
}
