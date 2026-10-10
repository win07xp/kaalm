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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

func TestScrapeTargetsSkipsTerminatingPods(t *testing.T) {
	// A gateway that drains on shutdown stays Running for seconds after a
	// rollout; counting it would put its counters in one snapshot and not
	// the next.
	gone := metav1.Now()
	running := corev1.PodStatus{Phase: corev1.PodRunning}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "gw-new"}, Status: running},
		{ObjectMeta: metav1.ObjectMeta{Name: "gw-old", DeletionTimestamp: &gone}, Status: running},
		{ObjectMeta: metav1.ObjectMeta{Name: "gw-pending"}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
	}
	got := scrapeTargets(pods)
	if len(got) != 1 || got[0] != "gw-new" {
		t.Errorf("scrapeTargets = %v, want [gw-new]", got)
	}
}

func TestPodsChanged(t *testing.T) {
	snap := func(pods ...string) *snapshot { return &snapshot{metrics: map[string][]sample{}, pods: pods} }
	cases := []struct {
		name          string
		before, after *snapshot
		want          bool
	}{
		{"same pods", snap("a", "b"), snap("a", "b"), false},
		{"replaced", snap("a"), snap("b"), true},
		{"one more", snap("a"), snap("a", "b"), true},
		{"one fewer", snap("a", "b"), snap("a"), true},
		{"no before", nil, snap("a"), false},
	}
	for _, c := range cases {
		if got := podsChanged(c.before, c.after); got != c.want {
			t.Errorf("%s: podsChanged = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGatewayFiguresLeftOutWhenPodsChanged(t *testing.T) {
	// Counters from different Pods do not subtract: the leg keeps the
	// client's figures and marks the gateway-side ones unavailable.
	spec := legSpec{histogram: "h", counter: metricLLMRequests}
	before := &snapshot{metrics: map[string][]sample{
		metricProcessCPU:  {{labels: map[string]string{}, value: 40}},
		metricLLMRequests: {{labels: map[string]string{}, value: 900}},
	}, pods: []string{"gw-old", "gw-new"}}
	after := &snapshot{metrics: map[string][]sample{
		metricProcessCPU:  {{labels: map[string]string{}, value: 12}},
		metricLLMRequests: {{labels: map[string]string{}, value: 1000}},
	}, pods: []string{"gw-new"}}
	leg := gatewayLeg{}
	gatewayFigures(&leg, spec, before, after)
	if !leg.GatewayPodsChanged || leg.GatewayRequests != 0 || leg.GatewayCPUPerRequestMs != 0 {
		t.Errorf("leg = %+v, want gatewayPodsChanged and no gateway-side figures", leg)
	}

	after.pods = []string{"gw-old", "gw-new"}
	leg = gatewayLeg{}
	before.metrics[metricProcessCPU][0].value = 10
	gatewayFigures(&leg, spec, before, after)
	if leg.GatewayPodsChanged || leg.GatewayRequests != 100 || leg.GatewayCPUPerRequestMs != 20 {
		t.Errorf("leg = %+v, want 100 requests at 20 ms CPU each", leg)
	}
}
