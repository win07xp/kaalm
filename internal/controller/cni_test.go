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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

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
