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

import "testing"

func TestCPUPerRequestMs(t *testing.T) {
	cases := []struct {
		cpu, requests, want float64
	}{
		{1.2, 3000, 0.4},
		{1.2, 0, 0},
		{0.5, -1, 0},
		{0, 100, 0},
	}
	for _, c := range cases {
		if got := cpuPerRequestMs(c.cpu, c.requests); got != c.want {
			t.Errorf("cpuPerRequestMs(%v, %v) = %v, want %v", c.cpu, c.requests, got, c.want)
		}
	}
}

func TestCounterDeltaSumsProcessCPUAcrossReplicas(t *testing.T) {
	// Two gateway replicas each report their own process CPU; the merged
	// snapshot sums them, so the leg's CPU is the whole gateway's.
	before := &snapshot{metrics: map[string][]sample{}}
	after := &snapshot{metrics: map[string][]sample{}}
	before.metrics[metricProcessCPU] = []sample{{labels: map[string]string{}, value: 10}}
	after.metrics[metricProcessCPU] = []sample{{labels: map[string]string{}, value: 11.5}}
	if got := counterDelta(before, after, metricProcessCPU, nil); got != 1.5 {
		t.Errorf("counterDelta = %v, want 1.5", got)
	}
}
