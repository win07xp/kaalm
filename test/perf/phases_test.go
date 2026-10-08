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
	"reflect"
	"strings"
	"testing"
	"time"
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
	got := holdLoadgenArgs(f, 400, 6.6667, 3*time.Minute)
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

// TestSpreadHoldMatchesPlacement checks that the hold's loadgen, given the
// fleet's arguments, posts to the namespace the ramp put each agent in.
func TestSpreadHoldMatchesPlacement(t *testing.T) {
	f := fleet{phase: phaseNamespaces, prefix: "spread-", namespaces: spreadNamespaces("perf", 20)}
	args := holdLoadgenArgs(f, 400, 1, time.Minute)
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
