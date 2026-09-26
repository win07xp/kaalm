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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestRequireControlled(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "team", UID: "agent-uid"}}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "team"}}

	err := requireControlled(testClient.Scheme(), agent, np)
	cc, ok := asChildConflict(err)
	if !ok {
		t.Fatalf("unowned object: got %v, want a ChildConflictError", err)
	}
	if cc.Kind != "NetworkPolicy" || cc.Name != "a1" || cc.OwnerKind != "Agent" {
		t.Errorf("conflict = %+v", cc)
	}
	if want := `NetworkPolicy "a1" already exists and is not owned by this Agent`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if _, ok := asChildConflict(fmt.Errorf("wrapped: %w", err)); !ok {
		t.Error("a wrapped conflict must still be recognized")
	}
	if _, ok := asChildConflict(errors.New("other")); ok {
		t.Error("an unrelated error is not a conflict")
	}

	// A non-controller owner reference is not control.
	yes, no := true, false
	np.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kaalm.io/v1beta1", Kind: "Agent", Name: "a1", UID: "agent-uid", Controller: &no,
	}}
	if _, ok := asChildConflict(requireControlled(testClient.Scheme(), agent, np)); !ok {
		t.Error("a non-controller reference must not count as ownership")
	}
	np.OwnerReferences[0].Controller = &yes
	if err := requireControlled(testClient.Scheme(), agent, np); err != nil {
		t.Errorf("controlled object: %v", err)
	}
}

// platformPolicy is a namespace-wide default deny, the kind of object a
// tenant must not be able to rewrite by naming a workload after it.
func platformPolicy(name string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

func createPlatformPolicy(t *testing.T, name string) networkingv1.NetworkPolicySpec {
	t.Helper()
	np := platformPolicy(name)
	if err := testClient.Create(ctxT(), np); err != nil {
		t.Fatalf("create platform policy: %v", err)
	}
	return np.Spec
}

func getPolicy(t *testing.T, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &np); err != nil {
		t.Fatalf("get policy %s: %v", name, err)
	}
	return &np
}

func deletePolicy(t *testing.T, name string) {
	t.Helper()
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	if err := testClient.Delete(ctxT(), np); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete policy %s: %v", name, err)
	}
}

// expectUntouchedPolicy checks the platform policy keeps its spec and gains
// no controller.
func expectUntouchedPolicy(t *testing.T, name string, want networkingv1.NetworkPolicySpec) {
	t.Helper()
	np := getPolicy(t, name)
	if !equality.Semantic.DeepEqual(np.Spec, want) {
		t.Errorf("platform policy spec changed: %+v", np.Spec)
	}
	if ref := metav1.GetControllerOf(np); ref != nil {
		t.Errorf("platform policy was adopted by %s %s", ref.Kind, ref.Name)
	}
}

func expectReadyMessageHas(t *testing.T, conds []metav1.Condition, substr string) {
	t.Helper()
	c := condition(conds, kaalmv1beta1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, substr) {
		t.Errorf("Ready = %+v, want False with a message containing %q", c, substr)
	}
}

func TestAgent_UnownedNetworkPolicyIsChildConflict(t *testing.T) {
	const name = "own-np-agent"
	want := createPlatformPolicy(t, name)
	mkWorkloadClass(t, "wc-own-np", nil)
	mkWorkloadAgent(t, name, "wc-own-np", nil)
	markCertReady(t, name)

	expectAgentReadyReason(t, name, kaalmv1beta1.ReasonChildConflict)
	expectReadyMessageHas(t, getWorkloadAgent(t, name).Status.Conditions, `NetworkPolicy "`+name+`"`)
	// Several requeues pass while the conflict holds: nothing changes.
	time.Sleep(4 * gateRequeue)
	expectUntouchedPolicy(t, name, want)
	if pod := agentPod(t, name); pod != nil {
		t.Fatalf("an Agent with a child conflict must not get a Pod, found %s", pod.Name)
	}
	expectAgentReadyReason(t, name, kaalmv1beta1.ReasonChildConflict)

	// Removing the conflicting object lets the Agent recover on its own.
	deletePolicy(t, name)
	eventually(t, func() error {
		if agentPod(t, name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	eventually(t, func() error {
		c := condition(getWorkloadAgent(t, name).Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason == kaalmv1beta1.ReasonChildConflict {
			return errString("Agent has not left ChildConflict")
		}
		return nil
	})
	np := getPolicy(t, name)
	if ref := metav1.GetControllerOf(np); ref == nil || ref.Kind != "Agent" || ref.Name != name {
		t.Errorf("the recreated policy must be controlled by the Agent, got %+v", ref)
	}
}

func TestAgent_ChildConflictKeepsRunningPod(t *testing.T) {
	const name = "own-running-agent"
	mkWorkloadClass(t, "wc-own-running", nil)
	pod := provisionRunningAgent(t, name, "wc-own-running")

	// Hand the Agent's Service to someone else: strip the controller
	// reference and change the port, as a tenant-created object would look.
	eventually(t, func() error {
		var svc corev1.Service
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &svc); err != nil {
			return err
		}
		svc.OwnerReferences = nil
		svc.Spec.Ports = []corev1.ServicePort{{Name: "other", Port: 9999}}
		return testClient.Update(ctxT(), &svc)
	})

	expectAgentReadyReason(t, name, kaalmv1beta1.ReasonChildConflict)
	var got corev1.Service
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 9999 || metav1.GetControllerOf(&got) != nil {
		t.Errorf("foreign Service was modified: %+v", got)
	}
	if live := agentPod(t, name); live == nil || live.UID != pod.UID {
		t.Errorf("the running Pod must survive a child conflict")
	}
}

func TestAgent_UnownedFQDNPolicyIsNeitherUpdatedNorDeleted(t *testing.T) {
	const name = "own-fqdn-agent"
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(ciliumPolicyGVK)
	foreign.SetName(fqdnPolicyName(name))
	foreign.SetNamespace("default")
	foreign.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{},
		"egress":           []any{map[string]any{"toFQDNs": []any{map[string]any{"matchPattern": "*"}}}},
	}
	if err := testClient.Create(ctxT(), foreign); err != nil {
		t.Fatalf("create foreign policy: %v", err)
	}
	wantSpec := foreign.Object["spec"]

	mkWorkloadClass(t, "wc-own-fqdn", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	})
	mkWorkloadAgent(t, name, "wc-own-fqdn", nil)
	markCertReady(t, name)

	expectAgentReadyReason(t, name, kaalmv1beta1.ReasonChildConflict)
	expectReadyMessageHas(t, getWorkloadAgent(t, name).Status.Conditions, fqdnPolicyName(name))
	u, err := getFQDNPolicy(name)
	if err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(u.Object["spec"], wantSpec) {
		t.Errorf("foreign CiliumNetworkPolicy was updated: %v", u.Object["spec"])
	}
	if agentPod(t, name) != nil {
		t.Error("an Agent with a child conflict must not get a Pod")
	}

	// With no hosts the Agent would delete its own policy; a foreign one stays.
	setClassHosts(t, "wc-own-fqdn", nil)
	time.Sleep(4 * gateRequeue)
	if _, err := getFQDNPolicy(name); err != nil {
		t.Errorf("foreign CiliumNetworkPolicy must not be deleted: %v", err)
	}
	expectAgentReadyReason(t, name, kaalmv1beta1.ReasonChildConflict)
}

func TestTask_UnownedNetworkPolicyIsChildConflict(t *testing.T) {
	const name = "own-np-task"
	want := createPlatformPolicy(t, name)
	mkWorkloadClass(t, "tc-own-np", nil)
	mkTask(t, name, "tc-own-np", nil)
	eventually(t, func() error { return markCertReadyErr(name) })

	eventually(t, func() error {
		c := condition(getTask(t, name).Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonChildConflict {
			return errString("no ChildConflict yet")
		}
		return nil
	})
	expectReadyMessageHas(t, getTask(t, name).Status.Conditions, `NetworkPolicy "`+name+`"`)
	time.Sleep(4 * gateRequeue)
	expectUntouchedPolicy(t, name, want)
	if pod := taskPod(t, name); pod != nil {
		t.Fatalf("a task with a child conflict must not get a Pod, found %s", pod.Name)
	}

	deletePolicy(t, name)
	eventually(t, func() error {
		if taskPod(t, name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	eventually(t, func() error {
		c := condition(getTask(t, name).Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason == kaalmv1beta1.ReasonChildConflict {
			return errString("task has not left ChildConflict")
		}
		return nil
	})
	if ref := metav1.GetControllerOf(getPolicy(t, name)); ref == nil || ref.Kind != "AgentTask" {
		t.Errorf("the recreated policy must be controlled by the AgentTask, got %+v", ref)
	}
}
