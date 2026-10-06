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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// mkServiceQuota creates namespace ns with a "services" quota that admits
// no Service.
func mkServiceQuota(t *testing.T, ns string) {
	t.Helper()
	mkResourceQuota(t, ns, corev1.ResourceServices, func() client.Object {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "quota-probe-", Namespace: ns},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		}
	})
}

// expectReadyWriteRejected checks Ready=False ChildWriteRejected with a
// message containing every substr.
func expectReadyWriteRejected(conds []metav1.Condition, substrs ...string) error {
	c := condition(conds, kaalmv1beta1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonChildWriteRejected {
		return fmt.Errorf("Ready = %+v, want False %s", c, kaalmv1beta1.ReasonChildWriteRejected)
	}
	for _, s := range substrs {
		if !strings.Contains(c.Message, s) {
			return fmt.Errorf("Ready message = %q, want it to contain %q", c.Message, s)
		}
	}
	return nil
}

func getAgentIn(t *testing.T, ns, name string) *kaalmv1beta1.Agent {
	t.Helper()
	var got kaalmv1beta1.Agent
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	return &got
}

func mkAgentIn(t *testing.T, ns, name, className string) {
	t.Helper()
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: className},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), ag); err != nil {
		t.Fatalf("create agent: %v", err)
	}
}

// An Agent whose Service create the namespace quota refuses reports it in
// Ready with one Warning, creates no Pod, and continues once the quota is
// raised, with no edit to the Agent.
func TestAgent_ServiceCreateRejectedByQuota(t *testing.T) {
	const ns, name = "svc-reject-quota", "svc-quota-agent"
	mkServiceQuota(t, ns)
	mkWorkloadClass(t, "wc-svc-reject-quota", nil)
	mkAgentIn(t, ns, name, "wc-svc-reject-quota")
	eventually(t, func() error { return markCertReadyIn(ns, name) })

	wantName := `creating Service "` + name + `"`
	eventually(t, func() error {
		return expectReadyWriteRejected(getAgentIn(t, ns, name).Status.Conditions, wantName, "exceeded quota")
	})
	ag := getAgentIn(t, ns, name)
	if ag.Status.Phase == kaalmv1beta1.AgentRunning {
		t.Errorf("phase = Running, want a phase before Running")
	}
	if p := podIn(t, ns, "kaalm.io/agent", name); p != nil {
		t.Fatalf("pod %s exists, want none", p.Name)
	}
	expectEvent(t, "Agent", ns, name, kaalmv1beta1.ReasonChildWriteRejected, corev1.EventTypeWarning, wantName)
	time.Sleep(4 * notReadyRecheck)
	if n := eventCount(objectEvents(t, "Agent", ns, name, kaalmv1beta1.ReasonChildWriteRejected)); n != 1 {
		t.Fatalf("ChildWriteRejected events = %d, want 1", n)
	}
	if p := podIn(t, ns, "kaalm.io/agent", name); p != nil {
		t.Fatalf("pod %s exists, want none", p.Name)
	}

	setQuota(t, ns, corev1.ResourceServices, 1)
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/agent", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
}

// A Running Agent whose deleted Service cannot be re-created keeps its
// phase and its Pod, shows the rejection in Ready, and gets the Service back
// and Ready=True once the quota allows it.
func TestAgent_ChildWriteRejectedKeepsRunningPod(t *testing.T) {
	const ns, name = "svc-reject-running", "svc-running-agent"
	if err := testClient.Create(ctxT(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	mkWorkloadClass(t, "wc-svc-reject-running", nil)
	mkAgentIn(t, ns, name, "wc-svc-reject-running")
	eventually(t, func() error { return markCertReadyIn(ns, name) })
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/agent", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	markPodReady(t, podIn(t, ns, "kaalm.io/agent", name))
	eventually(t, func() error {
		if p := getAgentIn(t, ns, name).Status.Phase; p != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase = %s, want Running", p)
		}
		return nil
	})
	podUID := podIn(t, ns, "kaalm.io/agent", name).UID

	mkServiceQuota(t, ns)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := testClient.Delete(ctxT(), svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}

	eventually(t, func() error {
		got := getAgentIn(t, ns, name)
		if got.Status.Phase != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase = %s, want Running", got.Status.Phase)
		}
		return expectReadyWriteRejected(got.Status.Conditions, `creating Service "`+name+`"`, "exceeded quota")
	})
	if p := podIn(t, ns, "kaalm.io/agent", name); p == nil || p.UID != podUID {
		t.Fatalf("pod = %v, want the running Pod %s kept", p, podUID)
	}

	setQuota(t, ns, corev1.ResourceServices, 1)
	eventually(t, func() error {
		var got corev1.Service
		return testClient.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: name}, &got)
	})
	eventually(t, func() error {
		c := condition(getAgentIn(t, ns, name).Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "PodRunning" {
			return fmt.Errorf("Ready = %+v, want True PodRunning", c)
		}
		return nil
	})
	if p := podIn(t, ns, "kaalm.io/agent", name); p == nil || p.UID != podUID {
		t.Fatalf("pod = %v, want the running Pod %s kept", p, podUID)
	}
}

// rejectingWrites is an interceptor whose Update and Delete fail with
// Forbidden, as a policy webhook's denial does.
func rejectingWrites(notFoundOnDelete bool) interceptor.Funcs {
	denied := func(obj client.Object) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, obj.GetName(),
			errors.New("denied by policy webhook"))
	}
	return interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return denied(obj)
		},
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if notFoundOnDelete {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "objects"}, obj.GetName())
			}
			return denied(obj)
		},
	}
}

// expectWriteRejected checks err is a ChildWriteRejectedError for op and
// kind.
func expectWriteRejected(t *testing.T, err error, op, kind string) {
	t.Helper()
	cr, ok := asChildWriteRejected(err)
	if !ok {
		t.Fatalf("err = %v, want a ChildWriteRejectedError", err)
	}
	if cr.Op != op || cr.Kind != kind {
		t.Errorf("error = %+v, want %s %s", cr, op, kind)
	}
}

// An in-place update of the Agent's Service or NetworkPolicy that the API
// server refuses comes back as a rejected write.
func TestAgent_ChildUpdateRejected(t *testing.T) {
	scheme := testScheme(t)
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "upd", Namespace: "default", UID: "agent-uid"}}
	class := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	eff := effectiveAgentSpec{HealthPort: 8080, ServicePort: 8080}

	svc := desiredService(agent, eff)
	svc.Spec.Ports[0].Port = 9999
	np := desiredNetworkPolicy(agent, class, eff, "kaalm-system", DNSSelector{})
	np.Spec.PolicyTypes = nil
	for _, obj := range []client.Object{svc, np} {
		if err := controllerutil.SetControllerReference(agent, obj, scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, np).
		WithInterceptorFuncs(rejectingWrites(false)).Build()
	r := &AgentReconciler{Client: c, OperatorNamespace: "kaalm-system"}

	expectWriteRejected(t, r.ensureService(ctxT(), agent, eff), "updating", "Service")
	expectWriteRejected(t, r.ensureNetworkPolicy(ctxT(), agent, class, eff), "updating", "NetworkPolicy")
}
