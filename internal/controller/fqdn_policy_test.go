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
	"context"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// ---- desiredFQDNPolicy ----

func TestDesiredFQDNPolicy_Shape(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "team"}}
	u := desiredFQDNPolicy(agent, agentPodLabels(agent), []string{"b.example.com", "a.example.com", "b.example.com"})

	if u.GetAPIVersion() != "cilium.io/v2" || u.GetKind() != "CiliumNetworkPolicy" {
		t.Errorf("gvk = %s %s", u.GetAPIVersion(), u.GetKind())
	}
	if u.GetName() != "a1-fqdn" || u.GetNamespace() != "team" {
		t.Errorf("name = %s/%s", u.GetNamespace(), u.GetName())
	}

	sel, _, _ := unstructured.NestedStringMap(u.Object, "spec", "endpointSelector", "matchLabels")
	if !reflect.DeepEqual(sel, agentPodLabels(agent)) {
		t.Errorf("endpointSelector = %v, want the Pod labels %v", sel, agentPodLabels(agent))
	}

	egress, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
	if len(egress) != 2 {
		t.Fatalf("egress rules = %d, want 2", len(egress))
	}
	dns := egress[0].(map[string]any)
	eps := dns["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if eps["k8s:io.kubernetes.pod.namespace"] != "kube-system" || eps["k8s:k8s-app"] != "kube-dns" {
		t.Errorf("DNS endpoints = %v", eps)
	}
	port := dns["toPorts"].([]any)[0].(map[string]any)
	p0 := port["ports"].([]any)[0].(map[string]any)
	if p0["port"] != "53" || p0["protocol"] != "ANY" {
		t.Errorf("DNS port = %v", p0)
	}
	rule := port["rules"].(map[string]any)["dns"].([]any)[0].(map[string]any)
	if rule["matchPattern"] != "*" {
		t.Errorf("DNS rule = %v", rule)
	}

	fq := egress[1].(map[string]any)
	if _, ok := fq["toPorts"]; ok {
		t.Error("the toFQDNs rule must not restrict ports")
	}
	want := []any{
		map[string]any{"matchName": "a.example.com"},
		map[string]any{"matchName": "b.example.com"},
	}
	if !reflect.DeepEqual(fq["toFQDNs"], want) {
		t.Errorf("toFQDNs = %v, want sorted and unique %v", fq["toFQDNs"], want)
	}
}

func TestEnsureFQDNPolicy_UnsupportedTouchesNothing(t *testing.T) {
	// A nil client would panic on any call: unsupported must make none.
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "team"}}
	if err := ensureFQDNPolicy(context.Background(), nil, nil, agent, nil, []string{"x.example.com"}, false); err != nil {
		t.Fatal(err)
	}
}

// ---- envtest: Agent and AgentTask ----

func getFQDNPolicy(name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumPolicyGVK)
	err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: fqdnPolicyName(name)}, u)
	return u, err
}

func expectFQDNHosts(t *testing.T, name, ownerKind string, hosts ...string) {
	t.Helper()
	eventually(t, func() error {
		u, err := getFQDNPolicy(name)
		if err != nil {
			return err
		}
		ref := metav1.GetControllerOf(u)
		if ref == nil || ref.Kind != ownerKind || ref.Name != name {
			return errString("policy is not controlled by the " + ownerKind)
		}
		egress, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
		if len(egress) != 2 {
			return errString("want two egress rules")
		}
		var want []any
		for _, h := range hosts {
			want = append(want, map[string]any{"matchName": h})
		}
		if !reflect.DeepEqual(egress[1].(map[string]any)["toFQDNs"], want) {
			return errString("toFQDNs do not match")
		}
		return nil
	})
}

func setClassHosts(t *testing.T, class string, hosts []string) {
	t.Helper()
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: class}, &ac); err != nil {
			return err
		}
		ac.Spec.Network.Egress.AllowedHosts = hosts
		return testClient.Update(ctxT(), &ac)
	})
}

func TestAgent_FQDNPolicyFollowsAllowedHosts(t *testing.T) {
	mkWorkloadClass(t, "wc-fqdn", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	})
	mkWorkloadAgent(t, "fqdn-agent", "wc-fqdn", nil)
	markCertReady(t, "fqdn-agent")
	expectFQDNHosts(t, "fqdn-agent", "Agent", "api.example.com")

	// A changed host list updates the policy in place.
	setClassHosts(t, "wc-fqdn", []string{"z.example.com", "api.example.com"})
	expectFQDNHosts(t, "fqdn-agent", "Agent", "api.example.com", "z.example.com")

	// Clearing the hosts deletes the policy.
	setClassHosts(t, "wc-fqdn", nil)
	eventually(t, func() error {
		if _, err := getFQDNPolicy("fqdn-agent"); !apierrors.IsNotFound(err) {
			return errString("FQDN policy should be deleted")
		}
		return nil
	})
}

func TestAgent_NoHostsNoFQDNPolicy(t *testing.T) {
	mkWorkloadClass(t, "wc-nofqdn", nil)
	mkWorkloadAgent(t, "nofqdn-agent", "wc-nofqdn", nil)
	markCertReady(t, "nofqdn-agent")
	eventually(t, func() error {
		if agentPod(t, "nofqdn-agent") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	if _, err := getFQDNPolicy("nofqdn-agent"); !apierrors.IsNotFound(err) {
		t.Errorf("no allowedHosts must mean no FQDN policy, got %v", err)
	}
}

func TestAgentTask_FQDNPolicy(t *testing.T) {
	mkWorkloadClass(t, "wc-task-fqdn", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	})
	mkTask(t, "fqdn-task", "wc-task-fqdn", nil)
	eventually(t, func() error { return markCertReadyErr("fqdn-task") })
	expectFQDNHosts(t, "fqdn-task", "AgentTask", "api.example.com")
}
