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
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A rejected Pod create during a wake is reported in status, not returned:
// the Agent stays Resuming, Ready carries the API server's error, and the
// stale podName is cleared.
func TestAgent_PodCreateRejectedKeepsResuming(t *testing.T) {
	rejection := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "woken-",
		errors.New(`pod rejected: RuntimeClass "absent" not found`))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return rejection
		},
	}).Build()
	agent := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "woken", Namespace: "default"},
		Status:     kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentResuming, PodName: "old"},
	}
	r := &AgentReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	eff := effectiveAgentSpec{HealthPort: 8080, ServicePort: 8080}
	_, rejected, err := r.convergePod(context.Background(), agent, &kaalmv1beta1.AgentClass{}, eff, "woken-tls", false)
	if err != nil || !rejected {
		t.Fatalf("convergePod = (rejected %v, err %v), want (true, nil)", rejected, err)
	}
	if agent.Status.Phase != kaalmv1beta1.AgentResuming {
		t.Errorf("phase = %s, want Resuming", agent.Status.Phase)
	}
	ready := condition(agent.Status.Conditions, kaalmv1beta1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != kaalmv1beta1.ReasonPodCreateRejected ||
		ready.Message != rejection.Error() {
		t.Errorf("Ready = %+v, want False %s with the API server error", ready, kaalmv1beta1.ReasonPodCreateRejected)
	}
	if agent.Status.PodName != "" {
		t.Errorf("podName = %q, want empty", agent.Status.PodName)
	}
}

// expectReadyRejected checks Ready=False PodCreateRejected with a message
// containing substr.
func expectReadyRejected(conds []metav1.Condition, substr string) error {
	c := condition(conds, kaalmv1beta1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonPodCreateRejected {
		return fmt.Errorf("Ready = %+v, want False %s", c, kaalmv1beta1.ReasonPodCreateRejected)
	}
	if !strings.Contains(c.Message, substr) {
		return fmt.Errorf("Ready message = %q, want it to contain %q", c.Message, substr)
	}
	return nil
}

// A class naming a RuntimeClass the cluster lacks: the API server rejects
// the Pod create. The Agent stays Provisioning (not Degraded) with no Pod,
// Ready names the rejection, one Warning reports it, and the timed re-check
// creates the Pod once the RuntimeClass exists, with no edit to the Agent.
func TestAgent_MissingRuntimeClassLeavesNoPod(t *testing.T) {
	const rcName = "absent-sandbox"
	rcRef := rcName
	mkWorkloadClass(t, "wc-missing-rc", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Runtime.RuntimeClassName = &rcRef
	})
	mkWorkloadAgent(t, "missing-rc-agent", "wc-missing-rc", nil)
	markCertReady(t, "missing-rc-agent")

	// The API server's RuntimeClass admission is what rejects the Pod.
	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-rc-probe", Namespace: "default"},
		Spec: corev1.PodSpec{
			RuntimeClassName: &rcRef,
			Containers:       []corev1.Container{{Name: "c", Image: "registry.test/agents/demo:v1"}},
		},
	}
	err := testClient.Create(ctxT(), probe)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `RuntimeClass "`+rcName+`" not found`) {
		t.Fatalf("pod create with missing RuntimeClass: err = %v, want Forbidden RuntimeClass not found", err)
	}

	wantMsg := `RuntimeClass "` + rcName + `" not found`
	eventually(t, func() error {
		return expectReadyRejected(getWorkloadAgent(t, "missing-rc-agent").Status.Conditions, wantMsg)
	})
	consistently(t, 4*notReadyRecheck, func() error {
		ag := getWorkloadAgent(t, "missing-rc-agent")
		if ag.Status.Phase != kaalmv1beta1.AgentProvisioning {
			return fmt.Errorf("phase = %s, want Provisioning", ag.Status.Phase)
		}
		if c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionDegraded); c != nil {
			return fmt.Errorf("degraded condition = %+v, want none", c)
		}
		if ag.Status.PodName != "" {
			return fmt.Errorf("podName = %q, want empty", ag.Status.PodName)
		}
		if p := agentPod(t, "missing-rc-agent"); p != nil {
			return fmt.Errorf("pod %s exists, want none", p.Name)
		}
		return expectReadyRejected(ag.Status.Conditions, wantMsg)
	})
	expectEvent(t, "Agent", "default", "missing-rc-agent", kaalmv1beta1.ReasonPodCreateRejected,
		corev1.EventTypeWarning, wantMsg)
	if n := eventCount(objectEvents(t, "Agent", "default", "missing-rc-agent",
		kaalmv1beta1.ReasonPodCreateRejected)); n != 1 {
		t.Fatalf("PodCreateRejected events = %d, want 1", n)
	}

	// Recovery: the re-check notices the RuntimeClass without an edit.
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: rcName}, Handler: "runsc"}
	if err := testClient.Create(ctxT(), rc); err != nil {
		t.Fatalf("create runtimeclass: %v", err)
	}
	eventually(t, func() error {
		p := agentPod(t, "missing-rc-agent")
		if p == nil {
			return errString("no pod yet")
		}
		if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != rcName {
			t.Fatalf("pod runtimeClassName = %v, want %s", p.Spec.RuntimeClassName, rcName)
		}
		return nil
	})
	eventually(t, func() error {
		// PodProvisioning, then PodNotReady: envtest has no kubelet.
		c := condition(getWorkloadAgent(t, "missing-rc-agent").Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || (c.Reason != "PodProvisioning" && c.Reason != "PodNotReady") {
			return fmt.Errorf("Ready = %+v, want PodProvisioning or PodNotReady", c)
		}
		return nil
	})
}

// mkQuotaNamespace creates namespace ns with a ResourceQuota "pods" that
// admits no Pod. It returns once a probe Pod create is refused with
// "exceeded quota".
func mkQuotaNamespace(t *testing.T, ns string) {
	t.Helper()
	mkResourceQuota(t, ns, corev1.ResourcePods, func() client.Object {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "quota-probe-", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.test/agents/demo:v1"}}},
		}
	})
}

// mkResourceQuota creates namespace ns (if needed) with a ResourceQuota,
// named after res, that admits none of res. envtest runs ResourceQuota
// admission but no quota controller, so the test writes the quota's status
// itself. It returns once creating probe() is refused with "exceeded quota".
func mkResourceQuota(t *testing.T, ns string, res corev1.ResourceName, probe func() client.Object) {
	t.Helper()
	if err := testClient.Create(ctxT(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	q := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: string(res), Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{res: resource.MustParse("0")}},
	}
	if err := testClient.Create(ctxT(), q); err != nil {
		t.Fatalf("create quota: %v", err)
	}
	setQuota(t, ns, res, 0)
	eventually(t, func() error {
		obj := probe()
		err := testClient.Create(ctxT(), obj)
		if err == nil {
			_ = testClient.Delete(ctxT(), obj)
			return errString("probe admitted, quota not enforced yet")
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "exceeded quota") {
			return fmt.Errorf("probe create: %v, want Forbidden exceeded quota", err)
		}
		return nil
	})
}

// setPodQuota sets the "pods" quota's hard limit in spec and status, with
// no Pod used.
func setPodQuota(t *testing.T, ns string, pods int) {
	t.Helper()
	setQuota(t, ns, corev1.ResourcePods, pods)
}

// setQuota sets the hard limit of the quota named after res, in spec and
// status, with none of res used.
func setQuota(t *testing.T, ns string, res corev1.ResourceName, n int) {
	t.Helper()
	hard := corev1.ResourceList{res: *resource.NewQuantity(int64(n), resource.DecimalSI)}
	eventually(t, func() error {
		var q corev1.ResourceQuota
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: string(res)}, &q); err != nil {
			return err
		}
		q.Spec.Hard = hard
		if err := testClient.Update(ctxT(), &q); err != nil {
			return err
		}
		q.Status.Hard = hard
		q.Status.Used = corev1.ResourceList{res: resource.MustParse("0")}
		return testClient.Status().Update(ctxT(), &q)
	})
}

// podIn fetches the non-terminating Pod in ns labeled key=name, or nil.
func podIn(t *testing.T, ns, key, name string) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := testClient.List(ctxT(), &pods, client.InNamespace(ns),
		client.MatchingLabels(map[string]string{key: name})); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			return &pods.Items[i]
		}
	}
	return nil
}

// A running Agent whose Pod is deleted while its namespace is over quota
// moves to Provisioning with Ready=False PodCreateRejected and one Warning,
// and gets its Pod back once the quota is raised, with no edit.
func TestAgent_PodCreateRejectedByQuota(t *testing.T) {
	const ns, name = "pod-reject-quota", "quota-agent"
	if err := testClient.Create(ctxT(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	mkWorkloadClass(t, "wc-reject-quota", nil)
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "wc-reject-quota"},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), ag); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	getAgent := func() *kaalmv1beta1.Agent {
		var got kaalmv1beta1.Agent
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			t.Fatalf("get agent: %v", err)
		}
		return &got
	}
	eventually(t, func() error { return markCertReadyIn(ns, name) })
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/agent", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	markPodReady(t, podIn(t, ns, "kaalm.io/agent", name))
	eventually(t, func() error {
		if p := getAgent().Status.Phase; p != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase = %s, want Running", p)
		}
		return nil
	})

	mkQuotaNamespace(t, ns)
	forceDeletePod(t, podIn(t, ns, "kaalm.io/agent", name))

	eventually(t, func() error {
		got := getAgent()
		if got.Status.Phase != kaalmv1beta1.AgentProvisioning {
			return fmt.Errorf("phase = %s, want Provisioning", got.Status.Phase)
		}
		if got.Status.PodName != "" {
			return fmt.Errorf("podName = %q, want empty", got.Status.PodName)
		}
		return expectReadyRejected(got.Status.Conditions, "exceeded quota")
	})
	expectEvent(t, "Agent", ns, name, kaalmv1beta1.ReasonPhaseChanged, corev1.EventTypeNormal,
		"from Running to Provisioning")
	expectEvent(t, "Agent", ns, name, kaalmv1beta1.ReasonPodCreateRejected, corev1.EventTypeWarning,
		"exceeded quota")
	time.Sleep(4 * notReadyRecheck)
	if n := eventCount(objectEvents(t, "Agent", ns, name, kaalmv1beta1.ReasonPodCreateRejected)); n != 1 {
		t.Fatalf("PodCreateRejected events = %d, want 1", n)
	}

	setPodQuota(t, ns, 1)
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/agent", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
}

// A class edit onto a missing RuntimeClass replaces the Agent's Pod, and
// the replacement is rejected: the Agent keeps its drift slot (Replacing)
// and shows Provisioning with Ready=False PodCreateRejected, not Failed.
func TestAgent_DriftOntoMissingRuntimeClass(t *testing.T) {
	mkWorkloadClass(t, "wc-drift-missing-rc", nil)
	pod := provisionRunningAgent(t, "drift-missing-rc", "wc-drift-missing-rc")

	missing := "absent-drift-sandbox"
	updateWorkloadClass(t, "wc-drift-missing-rc", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Runtime.RuntimeClassName = &missing
	})
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if got.DeletionTimestamp.IsZero() {
			return errString("old pod not yet marked for deletion")
		}
		forceDeletePod(t, &got)
		return nil
	})
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "drift-missing-rc")
		if ag.Status.Phase != kaalmv1beta1.AgentProvisioning {
			return fmt.Errorf("phase = %s, want Provisioning", ag.Status.Phase)
		}
		if c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionPodUpToDate); c == nil ||
			c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonReplacing {
			return fmt.Errorf("PodUpToDate = %+v, want False Replacing", c)
		}
		if ag.Status.PodName != "" {
			return fmt.Errorf("podName = %q, want empty", ag.Status.PodName)
		}
		return expectReadyRejected(ag.Status.Conditions, `RuntimeClass "`+missing+`" not found`)
	})
}
