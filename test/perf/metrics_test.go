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

// The parser reads both the controller-runtime and the gateway exposition,
// HELP and TYPE lines included.
func TestParseMetrics(t *testing.T) {
	body := []byte("# HELP kaalm_gateway_requests_total Requests.\n" +
		"# TYPE kaalm_gateway_requests_total counter\n" +
		"kaalm_gateway_requests_total{status=\"ok\"} 42\n")
	fams, err := parseMetrics(body)
	if err != nil {
		t.Fatal(err)
	}
	mf := fams["kaalm_gateway_requests_total"]
	if mf == nil || len(mf.Metric) != 1 || mf.Metric[0].GetCounter().GetValue() != 42 {
		t.Fatalf("parsed %v, want one counter at 42", fams)
	}
}

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

func TestCountGoroutinesInProfile(t *testing.T) {
	profile := []byte("goroutine profile: total 9\n" +
		"3 @ 0x43a1 0x44b2 0x4c03\n" +
		"#\t0x4a1b2c\tk8s.io/apimachinery/pkg/watch.(*StreamWatcher).receive+0x8c\t/go/watch/streamwatcher.go:110\n" +
		"\n" +
		"1 @ 0x43a1 0x4d00\n" +
		"#\t0x4b0000\tnet/http.(*persistConn).readLoop+0x10\t/go/net/http/transport.go:2200\n" +
		"#\t0x4a1b2c\tk8s.io/apimachinery/pkg/watch.(*StreamWatcher).receive+0x8c\t/go/watch/streamwatcher.go:110\n" +
		"\n" +
		"5 @ 0x43a1 0x4e00\n" +
		"#\t0x4c0000\tsigs.k8s.io/controller-runtime/pkg/internal/controller.(*Controller).Start+0x20\t/x.go:1\n")
	if got := countGoroutines(profile, watchGoroutine); got != 4 {
		t.Errorf("countGoroutines = %d, want 4", got)
	}
	if got := countGoroutines(nil, watchGoroutine); got != 0 {
		t.Errorf("empty profile: %d, want 0", got)
	}
}

func TestSecretWatchesSumsScopes(t *testing.T) {
	body := []byte("# TYPE apiserver_longrunning_requests gauge\n" +
		`apiserver_longrunning_requests{component="apiserver",group="",resource="secrets",scope="namespace",` +
		`subresource="",verb="WATCH",version="v1"} 12` + "\n" +
		`apiserver_longrunning_requests{component="apiserver",group="",resource="secrets",scope="cluster",` +
		`subresource="",verb="WATCH",version="v1"} 3` + "\n" +
		`apiserver_longrunning_requests{component="apiserver",group="",resource="pods",scope="namespace",` +
		`subresource="",verb="WATCH",version="v1"} 9` + "\n")
	fams, err := parseMetrics(body)
	if err != nil {
		t.Fatal(err)
	}
	s := &snapshot{metrics: map[string][]sample{}}
	s.merge(fams)
	if n, ok := secretWatches(s); !ok || n != 15 {
		t.Errorf("secretWatches = %v, %v; want 15, true", n, ok)
	}
	if n := apiserverFiguresFrom(s).SecretWatches; n == nil || *n != 15 {
		t.Errorf("apiserver figures: secretWatches %v, want 15", n)
	}
	empty := &snapshot{metrics: map[string][]sample{}}
	if _, ok := secretWatches(empty); ok {
		t.Error("an apiserver without the gauge: found true, want false")
	}
	if apiserverFiguresFrom(empty).SecretWatches != nil {
		t.Error("an apiserver without the gauge: secretWatches set, want left out")
	}
}

func TestLeaderPodName(t *testing.T) {
	if got := leaderPodName("kaalm-controller-7b4b478585-k9qmd_0f3a2c1e-9d7a-4b1e-8f00-1c2d3e4f5a6b"); got !=
		"kaalm-controller-7b4b478585-k9qmd" {
		t.Errorf("leaderPodName = %q", got)
	}
	if got := leaderPodName("plain"); got != "plain" {
		t.Errorf("a holder without an underscore: %q", got)
	}
}

func TestProcessFiguresFromFamilies(t *testing.T) {
	body := []byte("# TYPE go_goroutines gauge\ngo_goroutines 321\n" +
		"# TYPE go_memstats_heap_inuse_bytes gauge\ngo_memstats_heap_inuse_bytes 20971520\n" +
		"# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes 104857600\n" +
		"# TYPE rest_client_requests_total counter\n" +
		`rest_client_requests_total{code="200",host="10.43.0.1:443",method="GET"} 40` + "\n" +
		`rest_client_requests_total{code="404",host="10.43.0.1:443",method="GET"} 2` + "\n" +
		`rest_client_requests_total{code="200",host="10.43.0.1:443",method="PATCH"} 7` + "\n")
	fams, err := parseMetrics(body)
	if err != nil {
		t.Fatal(err)
	}
	got := processFiguresFrom("kaalm-gateway-abc", fams)
	want := processFigures{Pod: "kaalm-gateway-abc", Goroutines: 321, HeapMiB: 20, RSSMiB: 100, APIGets: 42}
	if got.Pod != want.Pod || got.Goroutines != want.Goroutines || got.HeapMiB != want.HeapMiB ||
		got.RSSMiB != want.RSSMiB || got.APIGets != want.APIGets || got.Watches != nil {
		t.Errorf("figures = %+v, want %+v", got, want)
	}
}
