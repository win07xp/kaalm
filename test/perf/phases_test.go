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
