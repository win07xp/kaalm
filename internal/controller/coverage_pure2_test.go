/*
Copyright 2026.

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

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- validateCallbackURL (rule 22, reconcile-time half) ----

func TestValidateCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantBad bool
	}{
		{"not https", "http://example.com/hook", true},
		{"parse error", "://no-scheme", true},
		{"empty host", "https://", true},
		{"loopback literal", "https://127.0.0.1/hook", true},
		{"public literal ok", "https://8.8.8.8/hook", false},
		{"unresolvable deferred", "https://nonexistent.invalid/hook", false},
	}
	for _, c := range cases {
		reason, _ := validateCallbackURL(c.url)
		bad := reason != ""
		if bad != c.wantBad {
			t.Errorf("%s: validateCallbackURL(%q) bad=%v, want %v (reason=%q)", c.name, c.url, bad, c.wantBad, reason)
		}
		if bad && reason != agentryv1alpha1.ReasonInvalidCallbackURL {
			t.Errorf("%s: reason=%q, want InvalidCallbackURL", c.name, reason)
		}
	}
}

// ---- HTTPProviderHealthChecker.Probe classification ----

func TestHTTPProbe_Classification(t *testing.T) {
	// anthropic path sets the x-api-key header; a 200 is Healthy.
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer anthropic.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	checker := &HTTPProviderHealthChecker{}
	probe := func(typ, endpoint string) ProviderProbeResult {
		return checker.Probe(context.Background(), &agentryv1alpha1.ModelProvider{
			Spec: agentryv1alpha1.ModelProviderSpec{Type: typ, Endpoint: endpoint},
		}, "sk-test")
	}

	if res := probe("anthropic", anthropic.URL); !res.Healthy {
		t.Errorf("anthropic 200 should be Healthy: %+v", res)
	}
	if res := probe("openai", unauthorized.URL); !res.AuthFailed {
		t.Errorf("401 should be AuthFailed: %+v", res)
	}
	if res := probe("openai-compatible", serverErr.URL); res.Err == nil {
		t.Errorf("500 should be a transient Err: %+v", res)
	}
	if res := probe("google-vertex", anthropic.URL); !res.Skipped {
		t.Errorf("google-vertex should be Skipped: %+v", res)
	}
	if res := probe("mystery-provider", anthropic.URL); res.Err == nil {
		t.Errorf("unknown type should error: %+v", res)
	}
}

// ---- ProbeFQDNPolicySupport ----

type fakeDiscovery struct {
	discovery.DiscoveryInterface
	groups *metav1.APIGroupList
	err    error
}

func (f fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) { return f.groups, f.err }

func TestProbeFQDNPolicySupport(t *testing.T) {
	// A CNI group present -> supported.
	yes := fakeDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: "apps"}, {Name: "cilium.io"},
	}}}
	if ok, err := ProbeFQDNPolicySupport(yes); err != nil || !ok {
		t.Errorf("cilium.io present should be supported: %v %v", ok, err)
	}

	// No CNI group -> unsupported.
	no := fakeDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "apps"}}}}
	if ok, err := ProbeFQDNPolicySupport(no); err != nil || ok {
		t.Errorf("no CNI group should be unsupported: %v %v", ok, err)
	}

	// A hard discovery failure (nil groups) is fatal.
	if _, err := ProbeFQDNPolicySupport(fakeDiscovery{groups: nil, err: errString("down")}); err == nil {
		t.Error("nil groups with error should be fatal")
	}
}

func TestFqdnSupport_ErrorPropagates(t *testing.T) {
	// fqdnSupport surfaces a discovery error without caching.
	r := &AgentClassReconciler{Discovery: fakeDiscovery{groups: nil, err: errString("discovery down")}}
	if _, err := r.fqdnSupport(); err == nil {
		t.Error("fqdnSupport must propagate the discovery error")
	}
}

// ---- deriveEffectiveTaskSpec ----

func TestDeriveEffectiveTaskSpec_ClassDefaults(t *testing.T) {
	sc := testStorageClass
	rc := "gvisor"
	class := &agentryv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: agentryv1alpha1.AgentClassSpec{
			Image: agentryv1alpha1.AgentClassImage{DefaultImage: "reg/default:v1", PullPolicy: corev1.PullAlways},
			Resources: agentryv1alpha1.AgentClassResources{
				Defaults: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			},
			Persistence: agentryv1alpha1.AgentClassPersistence{
				DefaultSizeGi: 5, MaxSizeGi: 10, StorageClassName: &sc,
			},
			Runtime: agentryv1alpha1.AgentClassRuntime{RuntimeClassName: &rc},
		},
	}
	// Task with no image and no resources inherits class defaults; size clamps.
	size := int32(50)
	task := &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
		Spec: agentryv1alpha1.AgentTaskSpec{
			Persistence: agentryv1alpha1.AgentTaskPersistence{Enabled: true, SizeGi: &size},
			Providers: []agentryv1alpha1.AgentProviderReference{
				{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "p1"}},
			},
		},
	}
	eff := deriveEffectiveTaskSpec(task, class)
	if eff.Image != "reg/default:v1" {
		t.Errorf("class default image not applied: %q", eff.Image)
	}
	if cpu := eff.Resources.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("100m")) != 0 {
		t.Error("class default resources not applied")
	}
	if eff.PVCSizeGi != 10 {
		t.Errorf("size not clamped to class max: %d", eff.PVCSizeGi)
	}
	if eff.RuntimeClassName == nil || *eff.RuntimeClassName != "gvisor" {
		t.Error("runtimeClassName not propagated")
	}
	if eff.PullPolicy != corev1.PullAlways {
		t.Errorf("pull policy not propagated: %v", eff.PullPolicy)
	}
	if len(eff.Providers) != 1 || eff.Providers[0] != "p1" {
		t.Errorf("providers not derived: %v", eff.Providers)
	}

	// Task overrides win; a default size applies when unset.
	task.Spec.Image = "reg/custom:v2"
	task.Spec.Persistence.SizeGi = nil
	eff = deriveEffectiveTaskSpec(task, class)
	if eff.Image != "reg/custom:v2" {
		t.Errorf("task image override lost: %q", eff.Image)
	}
	if eff.PVCSizeGi != 5 {
		t.Errorf("class default size not applied: %d", eff.PVCSizeGi)
	}
}
