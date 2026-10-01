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

package controller

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// ---- ProbeFQDNPolicySupport ----

// fakeDiscovery serves the API resources in resources, keyed by group
// version. A group version it does not serve is a NotFound error, as the API
// server returns; err, when set, replaces every answer.
type fakeDiscovery struct {
	discovery.DiscoveryInterface
	resources map[string][]string
	err       error
}

func (f fakeDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if f.err != nil {
		return nil, f.err
	}
	names, ok := f.resources[gv]
	if !ok {
		gvr := schema.GroupVersionResource{Group: gv}
		return nil, apierrors.NewNotFound(gvr.GroupResource(), "")
	}
	list := &metav1.APIResourceList{GroupVersion: gv}
	for _, n := range names {
		list.APIResources = append(list.APIResources, metav1.APIResource{Name: n})
	}
	return list, nil
}

// ServerGroups lists the groups of the served group versions.
func (f fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	if f.err != nil {
		return nil, f.err
	}
	list := &metav1.APIGroupList{}
	seen := map[string]bool{}
	for gv := range f.resources {
		g, err := schema.ParseGroupVersion(gv)
		if err != nil || seen[g.Group] {
			continue
		}
		seen[g.Group] = true
		list.Groups = append(list.Groups, metav1.APIGroup{Name: g.Group})
	}
	return list, nil
}

func TestProbeFQDNPolicySupport(t *testing.T) {
	// Cilium serves ciliumnetworkpolicies in cilium.io/v2 -> supported.
	yes := fakeDiscovery{resources: map[string][]string{
		"apps/v1":      {"deployments"},
		"cilium.io/v2": {"ciliumendpoints", "ciliumnetworkpolicies"},
	}}
	if ok, err := ProbeFQDNPolicySupport(yes); err != nil || !ok {
		t.Errorf("cilium.io/v2 ciliumnetworkpolicies should be supported: %v %v", ok, err)
	}

	// Tetragon installs TracingPolicy in cilium.io/v1alpha1 on any CNI, so
	// the cilium.io group alone is not Cilium (#273).
	tetragon := fakeDiscovery{resources: map[string][]string{
		"apps/v1":            {"deployments"},
		"cilium.io/v1alpha1": {"tracingpolicies", "tracingpoliciesnamespaced"},
	}}
	if ok, err := ProbeFQDNPolicySupport(tetragon); err != nil || ok {
		t.Errorf("cilium.io without ciliumnetworkpolicies should be unsupported: %v %v", ok, err)
	}

	// cilium.io/v2 served without the policy resource -> unsupported.
	noPolicy := fakeDiscovery{resources: map[string][]string{
		"cilium.io/v2": {"ciliumendpoints"},
	}}
	if ok, err := ProbeFQDNPolicySupport(noPolicy); err != nil || ok {
		t.Errorf("cilium.io/v2 without ciliumnetworkpolicies should be unsupported: %v %v", ok, err)
	}

	// Calico's CRD group exists on open-source Calico, which has no
	// domain-based egress, and Kaalm writes no Calico policy (#273).
	calico := fakeDiscovery{resources: map[string][]string{
		"apps/v1":                  {"deployments"},
		"crd.projectcalico.org/v1": {"networkpolicies"},
	}}
	if ok, err := ProbeFQDNPolicySupport(calico); err != nil || ok {
		t.Errorf("crd.projectcalico.org alone should be unsupported: %v %v", ok, err)
	}

	// No CNI group -> unsupported.
	no := fakeDiscovery{resources: map[string][]string{"apps/v1": {"deployments"}}}
	if ok, err := ProbeFQDNPolicySupport(no); err != nil || ok {
		t.Errorf("no CNI group should be unsupported: %v %v", ok, err)
	}

	// Any other discovery failure is an error, not an answer.
	if _, err := ProbeFQDNPolicySupport(fakeDiscovery{err: errString("down")}); err == nil {
		t.Error("a discovery failure should be an error")
	}
}

func TestFqdnSupport_ErrorPropagates(t *testing.T) {
	// fqdnSupport surfaces a discovery error without caching.
	p := NewFQDNProbe(fakeDiscovery{err: errString("discovery down")})
	r := &AgentClassReconciler{FQDNSupport: p.Supported}
	if _, err := r.fqdnSupport(); err == nil {
		t.Error("fqdnSupport must propagate the discovery error")
	}
	// A nil function is unsupported, not an error.
	if ok, err := (&AgentClassReconciler{}).fqdnSupport(); err != nil || ok {
		t.Errorf("nil FQDNSupport should be unsupported: %v %v", ok, err)
	}
}

// countingDiscovery counts discovery calls and fails while err is set.
type countingDiscovery struct {
	discovery.DiscoveryInterface
	calls int
	err   error
}

func (c *countingDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &metav1.APIResourceList{GroupVersion: gv,
		APIResources: []metav1.APIResource{{Name: "ciliumnetworkpolicies"}}}, nil
}

func TestFQDNProbe_Caches(t *testing.T) {
	dc := &countingDiscovery{err: errString("down")}
	p := NewFQDNProbe(dc)
	// A failed probe is not cached.
	if _, err := p.Supported(); err == nil {
		t.Fatal("first probe should fail")
	}
	dc.err = nil
	for range 3 {
		if ok, err := p.Supported(); err != nil || !ok {
			t.Fatalf("probe should report supported: %v %v", ok, err)
		}
	}
	if dc.calls != 2 {
		t.Errorf("discovery called %d times, want 2 (one failure, then cached)", dc.calls)
	}
}
