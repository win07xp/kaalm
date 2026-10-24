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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestManyProvidersAndClasses(t *testing.T) {
	providers := manyProviders("perf", 50)
	if len(providers) != 50 || providers[0].Name != "perf-many-00" || providers[49].Name != "perf-many-49" {
		t.Fatalf("providers: %d, first %s", len(providers), providers[0].Name)
	}
	for _, p := range []int{0, 7, 49} {
		mp := providers[p]
		if mp.Labels[phaseLabel] != phaseProviders {
			t.Errorf("%s: phase label %q", mp.Name, mp.Labels[phaseLabel])
		}
		if mp.Spec.HealthCheck == nil || mp.Spec.HealthCheck.Enabled {
			t.Errorf("%s: health probes must be off", mp.Name)
		}
		if len(mp.Spec.AllowedNamespaces) != 1 || mp.Spec.AllowedNamespaces[0] != "perf" {
			t.Errorf("%s: allowedNamespaces %v", mp.Name, mp.Spec.AllowedNamespaces)
		}
	}
	// Distinct endpoint prefixes keep the mock's per-prefix counters apart.
	seen := map[string]bool{}
	for _, mp := range providers {
		if seen[mp.Spec.Endpoint] {
			t.Errorf("endpoint %s repeats", mp.Spec.Endpoint)
		}
		seen[mp.Spec.Endpoint] = true
	}

	classes := manyClasses(providers)
	if len(classes) != 50 || classes[0].Name != "perf-many-00" {
		t.Fatalf("classes: %d", len(classes))
	}
	// Every class names every provider: the widest watch fan-out.
	for _, c := range []int{0, 49} {
		if got := len(classes[c].Spec.AllowedProviders); got != 50 {
			t.Errorf("class %d allows %d providers, want 50", c, got)
		}
		if classes[c].Labels[phaseLabel] != phaseProviders {
			t.Errorf("class %d: phase label %q", c, classes[c].Labels[phaseLabel])
		}
	}
}

// TestHoldArgsForOneNamespace pins the standard hold's loadgen arguments,
// which the refactor over fleets must leave as they were.
func TestHoldArgsForOneNamespace(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	got := holdLoadgenArgs(f, 400, 1, 6.6667, 3*time.Minute)
	want := []string{
		"-mode", "channels", "-path-prefix", "/channels/perf/ramp-", "-count", "400",
		"-pad", "4", "-rate", "6.6667", "-duration", "3m0s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hold args = %v, want %v", got, want)
	}
}

// TestGatewayLegSpecsKeepJobsAndArgs pins the gateway table's job names and
// loadgen arguments through the shared leg code.
func TestGatewayLegSpecsKeepJobsAndArgs(t *testing.T) {
	jobs := []string{
		"loadgen-gateway-token-perf-fast", "loadgen-gateway-mtls-perf-fast", "loadgen-gateway-mtls-perf-slow",
		"loadgen-gateway-mtls-perf-hard", "loadgen-gateway-mtls-perf-limited",
	}
	if len(gatewayLegs) != len(jobs) {
		t.Fatalf("%d gateway legs, want %d", len(gatewayLegs), len(jobs))
	}
	for i, d := range gatewayLegs {
		spec := gatewayLegSpec(d)
		if spec.job != jobs[i] || spec.mode != modeGateway || spec.mtls != d.mtls {
			t.Errorf("leg %d: job %q mode %q mtls %v", i, spec.job, spec.mode, spec.mtls)
		}
		if want := []string{"-model", d.provider + "/mock-model"}; !reflect.DeepEqual(spec.args, want) {
			t.Errorf("leg %d args = %v, want %v", i, spec.args, want)
		}
		if spec.histogram != metricLLMDuration || spec.counter != metricLLMRequests ||
			spec.labels["provider"] != d.provider {
			t.Errorf("leg %d reads %s/%s %v", i, spec.histogram, spec.counter, spec.labels)
		}
	}
}

func TestToolLegSpecs(t *testing.T) {
	want := []struct {
		job, provider string
		mtls, legacy  bool
	}{
		{"loadgen-tools-token", toolProviderName, false, false},
		{"loadgen-tools-mtls", toolProviderName, true, false},
		{"loadgen-tools-legacy-mtls", toolLegacyProviderName, true, true},
	}
	if len(toolLegSpecs) != len(want) {
		t.Fatalf("%d tool legs, want %d", len(toolLegSpecs), len(want))
	}
	for i, w := range want {
		spec := toolLegSpecs[i]
		if spec.job != w.job || spec.provider != w.provider || spec.mtls != w.mtls || spec.mode != modeTools {
			t.Errorf("leg %d: job %q provider %q mtls %v mode %q", i, spec.job, spec.provider, spec.mtls, spec.mode)
		}
		if spec.histogram != metricToolDuration || spec.counter != metricToolCalls ||
			!reflect.DeepEqual(spec.labels, map[string]string{"provider": w.provider, "tool": toolName}) {
			t.Errorf("leg %d reads %s/%s %v", i, spec.histogram, spec.counter, spec.labels)
		}
		args := strings.Join(spec.args, " ")
		if got := strings.Contains(args, "-legacy-session"); got != w.legacy {
			t.Errorf("leg %d args %q: -legacy-session %v, want %v", i, args, got, w.legacy)
		}
		if w.legacy && !strings.Contains(args, "-tool-url "+gatewayBase+"/v1/mcp/"+toolLegacyProviderName) {
			t.Errorf("leg %d args %q do not target %s", i, args, toolLegacyProviderName)
		}
	}
}

func TestToolsClassAndAgentCoverBothProviders(t *testing.T) {
	var allowed []string
	for _, ref := range toolsClass().Spec.AllowedToolProviders {
		allowed = append(allowed, ref.Name)
	}
	if want := []string{toolProviderName, toolLegacyProviderName}; !reflect.DeepEqual(allowed, want) {
		t.Errorf("class allows %v, want %v", allowed, want)
	}
	grants := map[string][]string{}
	for _, g := range toolsAgentObj("perf", "img").Spec.Tools {
		grants[g.ProviderRef.Name] = g.Tools
	}
	want := map[string][]string{toolProviderName: {toolName}, toolLegacyProviderName: {toolName}}
	if !reflect.DeepEqual(grants, want) {
		t.Errorf("agent grants %v, want %v", grants, want)
	}
}

// TestSpreadHoldMatchesPlacement checks that the hold's loadgen, given the
// fleet's arguments, posts to the namespace the ramp put each agent in.
func TestSpreadHoldMatchesPlacement(t *testing.T) {
	f := fleet{phase: phaseNamespaces, prefix: "spread-", namespaces: spreadNamespaces("perf", 20)}
	args := holdLoadgenArgs(f, 400, 1, 1, time.Minute)
	var prefix string
	var nss []string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-path-prefix":
			prefix = args[i+1]
		case "-namespaces":
			nss = strings.Split(args[i+1], ",")
		}
	}
	for _, i := range []int{0, 1, 19, 20, 21, 399} {
		want := "/channels/" + f.namespaceOf(i) + "/" + f.name(i)
		if got := channelURL("", prefix, nss, 4, i); got != want {
			t.Errorf("agent %d: loadgen posts to %s, fleet placed it at %s", i, got, want)
		}
	}
}

// A namespaces phase that fails while preparing its namespaces, after it has
// created them, still deletes them on the way out.
func TestNamespacesPhaseDeletesNamespacesWhenPrepareFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cmapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var seed []client.Object
	for _, name := range spreadSecrets {
		seed = append(seed, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "perf", Name: name}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.New("secret copy refused")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()

	cfg := config{Namespace: "perf", NamespacesCount: 3, NamespacesAgents: 6}
	h := &harness{cfg: cfg, k: &cluster{c: c}, sum: &summary{}}
	if err := h.runNamespaces(context.Background()); err == nil {
		t.Fatal("runNamespaces succeeded; want the Secret copy failure")
	}
	for _, name := range spreadNamespaces("perf", 3) {
		var ns corev1.Namespace
		err := c.Get(context.Background(), client.ObjectKey{Name: name}, &ns)
		if !apierrors.IsNotFound(err) {
			t.Errorf("namespace %s left behind after a failed prepare (get err %v)", name, err)
		}
	}
}

func TestKillingTimesForPods(t *testing.T) {
	at := func(sec, ms int) metav1.Time {
		return metav1.NewTime(time.Date(2026, 10, 20, 12, 0, sec, ms*int(time.Millisecond), time.FixedZone("x", 3600)))
	}
	event := func(pod, reason string, ts metav1.Time) corev1.Event {
		return corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod},
			Reason:         reason,
			LastTimestamp:  ts,
		}
	}
	events := []corev1.Event{
		event("kaalm-gateway-b", "Killing", at(30, 250)),
		event("kaalm-gateway-a", "Killing", at(10, 5)),
		event("kaalm-gateway-a", "Pulled", at(5, 0)),
		event("kaalm-controller-c", "Killing", at(1, 0)),
	}
	got := killingTimes(events, "kaalm-gateway-")
	want := []string{
		"kaalm-gateway-a 2026-10-20T11:00:10.005Z",
		"kaalm-gateway-b 2026-10-20T11:00:30.250Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("killingTimes = %q, want %q", got, want)
	}
}

func TestHoldArgsWithChannelsPerAgent(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	got := holdLoadgenArgs(f, 400, 2, 6.6667, 3*time.Minute)
	want := []string{
		"-mode", "channels", "-path-prefix", "/channels/perf/ramp-", "-count", "800",
		"-pad", "4", "-rate", "6.6667", "-duration", "3m0s", "-per-agent", "2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hold args = %v, want %v", got, want)
	}
}

func TestReadyRunStopsAtTheFirstGap(t *testing.T) {
	f := fleet{prefix: "ramp-"}
	ts := []agentTiming{
		{Name: "ramp-0000", ReadyNow: true}, {Name: "ramp-0001", ReadyNow: true},
		{Name: "ramp-0002"}, {Name: "ramp-0003", ReadyNow: true},
	}
	if got := readyRun(ts, f); got != 2 {
		t.Errorf("readyRun = %d, want 2", got)
	}
}

// The restart block's JSON keys are what the performance page documents.
func TestRestartResultJSONKeys(t *testing.T) {
	watches, secretWatches := 7, 15.0
	res := restartResult{
		Agents: 400, Channels: 800, ChannelSecrets: 1000, ControllerWarmupSec: 42,
		ControllerWarmupWatches: []watchSample{{AtSec: 2, SecretWatches: 10}},
		SweepBefore:             []sweepTarget{{Target: "kaalm-gateway-a", Channels: 800, Served: 800}},
		SweepAfter:              []sweepTarget{{Target: "kaalm-gateway-b", Channels: 800, Served: 799, Unserved: 1}},
		Failover:                &failoverResult{Killed: "ctl-a", NewLeader: "ctl-b", Lease: leaseReleased, WarmupSec: 30},
		Figures: map[string]scaleFigures{pointBefore: {
			Gateway:    []processFigures{{Pod: "kaalm-gateway-a", Watches: &watches}},
			Controller: []processFigures{{Pod: "ctl-a", Leader: true}},
			APIServer:  apiserverFigures{RSSMiB: 900, CPUSeconds: 12, SecretWatches: &secretWatches},
		}},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"agents", "controllerRolloutSec", "controllerFirstReconcileSec", "gatewayRolloutSec", "gatewayClient",
		"gatewayFailed", "channels", "channelSecrets", "controllerWarmupSec", "controllerWarmupWatches",
		"sweepBefore", "sweepAfter", "failover", "figures",
	} {
		if _, ok := top[key]; !ok {
			t.Errorf("restart JSON has no %q key: %s", key, raw)
		}
	}
	for _, want := range []string{
		`"served":800`, `"unserved":1`, `"servedSec"`, `"acceptMs"`, `"killed":"ctl-a"`, `"newLeader":"ctl-b"`,
		`"lease":"released"`,
		`"firstReconcileSec"`, `"warmupSec":30`, `"recoveredSec"`, `"before":{`, `"gateway":[`, `"controller":[`,
		`"watches":7`, `"leader":true`, `"heapMiB"`, `"rssMiB"`, `"apiGets"`, `"goroutines"`,
		`"apiserver":{"rssMiB":900,"cpuSeconds":12,"secretWatches":15}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("restart JSON lacks %s: %s", want, raw)
		}
	}
	skipped, err := json.Marshal(restartResult{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skipped), `"failover":null`) {
		t.Errorf("a skipped failover should read null: %s", skipped)
	}
	printRestart(&res) // the summary table must not trip over any field
}

func TestChannelsWarm(t *testing.T) {
	snap := func(reconciles, depth, active float64) *snapshot {
		body := fmt.Sprintf("# TYPE controller_runtime_reconcile_total counter\n"+
			"controller_runtime_reconcile_total{controller=\"agentchannel\",result=\"success\"} %v\n"+
			"controller_runtime_reconcile_total{controller=\"agent\",result=\"success\"} 9999\n"+
			"# TYPE workqueue_depth gauge\nworkqueue_depth{controller=\"agentchannel\",name=\"agentchannel\"} %v\n"+
			"# TYPE controller_runtime_active_workers gauge\n"+
			"controller_runtime_active_workers{controller=\"agentchannel\"} %v\n", reconciles, depth, active)
		fams, err := parseMetrics([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		s := &snapshot{metrics: map[string][]sample{}}
		s.merge(fams)
		return s
	}
	cases := []struct {
		name                      string
		reconciles, depth, active float64
		want                      bool
	}{
		{"every channel reconciled, queue empty", 400, 0, 0, true},
		{"fewer reconciles than channels", 399, 0, 0, false},
		{"queue not empty", 500, 3, 0, false},
		{"a worker still busy", 500, 0, 1, false},
	}
	for _, c := range cases {
		if got := channelsWarm(snap(c.reconciles, c.depth, c.active), 400); got != c.want {
			t.Errorf("%s: channelsWarm = %v, want %v", c.name, got, c.want)
		}
	}
}

// The restart sweep posts where the fleet serves each channel.
func TestSweepGatewayArgsMatchFleetChannels(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	args := sweepGatewayArgs(f, []string{"10.42.0.7", "10.42.1.9"}, 3, 2)
	flags := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	if flags["-mode"] != modeSweep || flags["-count"] != "6" || flags["-per-agent"] != "2" {
		t.Errorf("sweep args %v", args)
	}
	if flags["-targets"] != "https://10.42.0.7:8080,https://10.42.1.9:8080" ||
		flags["-server-name"] != "kaalm-gateway.kaalm-system.svc" {
		t.Errorf("targets %q, server name %q", flags["-targets"], flags["-server-name"])
	}
	c := loadgenConfig{pathPrefix: flags["-path-prefix"], pad: 4, perAgent: 2}
	cfg := config{ChannelsPerAgent: 2}
	for j := 0; j < 6; j++ {
		ch := f.channels(j/2, cfg, "https://cb")[j%2].(*kaalmv1beta1.AgentChannel)
		if got := channelJobURL(c, "", j); got != ch.Spec.Webhook.Path {
			t.Errorf("job %d: sweep posts to %s, fleet serves %s", j, got, ch.Spec.Webhook.Path)
		}
	}
}

// The failover's first-reconcile time and warm-up start do not wait for the
// Lease follower: a slow handoff still leaves both near the kill.
func TestFailoverWaitsDoNotWaitOnTheLeaseHandoff(t *testing.T) {
	const handoffDelay = 500 * time.Millisecond
	start := time.Now()
	handoff := make(chan handoffResult, 1)
	go func() {
		time.Sleep(handoffDelay)
		handoff <- handoffResult{outcome: leaseReleased}
	}()
	var warmupAt time.Duration
	res := &failoverResult{}
	leaseErr, err := timeFailoverWaits(start, res,
		func() error { return nil },
		func() (float64, []watchSample) {
			warmupAt = time.Since(start)
			return 1, nil
		},
		handoff)
	if err != nil || leaseErr != nil {
		t.Fatalf("timeFailoverWaits: err %v, lease err %v", err, leaseErr)
	}
	if limit := handoffDelay.Seconds() / 2; res.FirstReconcileSec >= limit {
		t.Errorf("firstReconcileSec = %.3f, want under %.3f: it waited on the Lease handoff", res.FirstReconcileSec, limit)
	}
	if warmupAt >= handoffDelay/2 {
		t.Errorf("warm-up started at %v, want under %v: it waited on the Lease handoff", warmupAt, handoffDelay/2)
	}
	if res.Lease != leaseReleased || res.WarmupSec != 1 {
		t.Errorf("result = %+v, want lease %q and warmupSec 1", res, leaseReleased)
	}
}

// A failed first reconcile is the failover's error; a failed Lease read is
// returned apart and leaves the timings and an empty outcome.
func TestFailoverWaitsErrors(t *testing.T) {
	handoff := make(chan handoffResult, 1)
	handoff <- handoffResult{}
	if _, err := timeFailoverWaits(time.Now(), &failoverResult{},
		func() error { return errors.New("no reconcile") },
		func() (float64, []watchSample) { t.Error("warm-up ran after a failed first reconcile"); return 0, nil },
		handoff); err == nil {
		t.Error("timeFailoverWaits succeeded; want the first-reconcile error")
	}

	handoff = make(chan handoffResult, 1)
	handoff <- handoffResult{outcome: leaseExpired, err: errors.New("lease read failed")}
	res := &failoverResult{}
	leaseErr, err := timeFailoverWaits(time.Now(), res,
		func() error { return nil },
		func() (float64, []watchSample) { return 2, nil },
		handoff)
	if err != nil || leaseErr == nil {
		t.Fatalf("err %v, lease err %v; want only a lease error", err, leaseErr)
	}
	if res.Lease != "" || res.WarmupSec != 2 {
		t.Errorf("result = %+v, want no lease outcome and warmupSec 2", res)
	}
}
