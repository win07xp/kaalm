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
	"fmt"
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// holdWindow is how long the envtest hold tests pin a held state. With the
// 5s certificate requeue and the watch-driven passes the tests trigger, it
// spans several reconcile passes.
const holdWindow = 2 * time.Second

// reissueCertificate deletes the Agent's Certificate, as an operator would
// to force a re-issue. The controller re-creates it without status, so it is
// not Ready until markCertReady, and the test waits for the Agent to report
// that.
func reissueCertificate(t *testing.T, agentName string) {
	t.Helper()
	key := types.NamespacedName{Namespace: "default", Name: agentCertificateName(agentName)}
	var cert cmapi.Certificate
	if err := testClient.Get(ctxT(), key, &cert); err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	if err := testClient.Delete(ctxT(), &cert); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete certificate: %v", err)
	}
	old := cert.UID
	eventually(t, func() error {
		var got cmapi.Certificate
		if err := testClient.Get(ctxT(), key, &got); err != nil {
			return err
		}
		if got.UID == old {
			return errString("certificate not yet re-created")
		}
		return nil
	})
	expectAgentReadyReason(t, agentName, kaalmv1beta1.ReasonCertificateNotReady)
}

// expectPodKept reads the Pod straight from the apiserver and fails unless it is
// the same Pod and not terminating.
func expectPodKept(name string, uid types.UID) error {
	var got corev1.Pod
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
		return err
	}
	if got.UID != uid {
		return fmt.Errorf("pod %s replaced", name)
	}
	if !got.DeletionTimestamp.IsZero() {
		return fmt.Errorf("pod %s is terminating", name)
	}
	return nil
}

// An Agent whose Certificate is re-issued keeps its serving Pod and its
// phase, converges its other children, and reports the wait on Ready.
func TestAgent_CertificateReissueKeepsServingPod(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-reissue", nil)
	pod := provisionRunningAgent(t, "cert-reissue", "wc-cert-reissue")
	expectDriftReason(t, "cert-reissue", kaalmv1beta1.ReasonPodCurrent)

	reissueCertificate(t, "cert-reissue")
	ready := condition(getWorkloadAgent(t, "cert-reissue").Status.Conditions, kaalmv1beta1.ConditionReady)
	if ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, "keeps serving") {
		t.Errorf("Ready = %+v, want False saying the current Pod keeps serving", ready)
	}
	consistently(t, holdWindow, func() error {
		if err := expectPodKept(pod.Name, pod.UID); err != nil {
			return err
		}
		ag := getWorkloadAgent(t, "cert-reissue")
		if ag.Status.Phase != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase=%s want Running", ag.Status.Phase)
		}
		if got := podUpToDateReason(ag); got != kaalmv1beta1.ReasonPodCurrent {
			return fmt.Errorf("PodUpToDate reason %q, want Current", got)
		}
		return nil
	})

	// The non-Pod children still converge during the hold.
	svcKey := types.NamespacedName{Namespace: "default", Name: "cert-reissue"}
	var svc corev1.Service
	if err := testClient.Get(ctxT(), svcKey, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if err := testClient.Delete(ctxT(), &svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	eventually(t, func() error {
		var got corev1.Service
		if err := testAPIReader.Get(ctxT(), svcKey, &got); err != nil {
			return err
		}
		if got.UID == svc.UID {
			return errString("service not yet re-created")
		}
		return nil
	})

	markCertReady(t, "cert-reissue")
	expectAgentReadyReason(t, "cert-reissue", kaalmv1beta1.ReasonPodRunning)
	if err := expectPodKept(pod.Name, pod.UID); err != nil {
		t.Fatal(err)
	}
}

// A drift found while the Certificate is not Ready waits for it without a
// drift slot, then replaces the Pod once the Certificate is Ready.
func TestAgent_CertificateHoldDefersDriftReplacement(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-drift", nil)
	oldPod := provisionRunningAgent(t, "cert-drift", "wc-cert-drift")
	oldHash := oldPod.Annotations[annotationPodSpecHash]
	reissueCertificate(t, "cert-drift")

	eventually(t, func() error {
		ag := getWorkloadAgent(t, "cert-drift")
		ag.Spec.Env = []corev1.EnvVar{{Name: "NEW_FLAG", Value: "on"}}
		return testClient.Update(ctxT(), ag)
	})
	expectDriftReason(t, "cert-drift", kaalmv1beta1.ReasonCertificateNotReady)
	consistently(t, holdWindow, func() error {
		if err := expectPodKept(oldPod.Name, oldPod.UID); err != nil {
			return err
		}
		if got := podUpToDateReason(getWorkloadAgent(t, "cert-drift")); got != kaalmv1beta1.ReasonCertificateNotReady {
			return fmt.Errorf("PodUpToDate reason %q, want CertificateNotReady", got)
		}
		return nil
	})

	markCertReady(t, "cert-drift")
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
		if apierrors.IsNotFound(err) || err == nil && got.UID != oldPod.UID {
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
	var fresh *corev1.Pod
	eventually(t, func() error {
		fresh = agentPod(t, "cert-drift")
		if fresh == nil || fresh.UID == oldPod.UID {
			return errString("no replacement pod yet")
		}
		if fresh.Annotations[annotationPodSpecHash] == oldHash {
			return errString("replacement pod has the old hash")
		}
		return nil
	})
	markPodReady(t, fresh)
	expectDriftReason(t, "cert-drift", kaalmv1beta1.ReasonPodCurrent)
	expectAgentPhase(t, "cert-drift", kaalmv1beta1.AgentRunning)
}

// An Idle Agent whose Certificate is not Ready keeps its Pod past the
// hibernation delay, because a wake could not create a new one, and
// hibernates once the Certificate is Ready.
func TestAgent_CertificateHoldKeepsIdlePod(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-idle", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	provisionRunningAgentWithLifecycle(t, "cert-idle", "wc-cert-idle", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
		ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Second}
	})
	pod := agentPod(t, "cert-idle")
	reissueCertificate(t, "cert-idle")

	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "cert-idle", 2*time.Hour)}, 1)
	touchAgent(t, "cert-idle")
	expectAgentPhase(t, "cert-idle", kaalmv1beta1.AgentIdle)
	touched := false
	start := time.Now()
	consistently(t, holdWindow, func() error {
		if !touched && time.Since(start) > holdWindow/2 {
			touchAgent(t, "cert-idle")
			touched = true
		}
		if err := expectPodKept(pod.Name, pod.UID); err != nil {
			return err
		}
		if ph := getWorkloadAgent(t, "cert-idle").Status.Phase; ph != kaalmv1beta1.AgentIdle {
			return fmt.Errorf("phase=%s want Idle", ph)
		}
		return nil
	})

	markCertReady(t, "cert-idle")
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, listAgentPods("cert-idle")...); err != nil {
			return err
		}
		for i := range pods.Items {
			if !pods.Items[i].DeletionTimestamp.IsZero() {
				forceDeletePod(t, &pods.Items[i])
			}
		}
		if ph := getWorkloadAgent(t, "cert-idle").Status.Phase; ph != kaalmv1beta1.AgentHibernated {
			return fmt.Errorf("phase=%s want Hibernated", ph)
		}
		return nil
	})
}

// A Running Agent that loses its Pod while its Certificate is not Ready
// moves to Provisioning and clears podName, and its new Pod waits for the
// Certificate.
func TestAgent_CertificateWaitAfterPodLossProvisions(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-lost", nil)
	pod := provisionRunningAgent(t, "cert-lost", "wc-cert-lost")
	reissueCertificate(t, "cert-lost")

	forceDeletePod(t, pod)
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "cert-lost")
		if ag.Status.Phase != kaalmv1beta1.AgentProvisioning || ag.Status.PodName != "" {
			return fmt.Errorf("phase=%s podName=%q, want Provisioning and no podName",
				ag.Status.Phase, ag.Status.PodName)
		}
		return nil
	})
	consistently(t, holdWindow, func() error {
		if p := agentPod(t, "cert-lost"); p != nil {
			return fmt.Errorf("pod %s created before the certificate is Ready", p.Name)
		}
		return nil
	})

	markCertReady(t, "cert-lost")
	eventually(t, func() error {
		if agentPod(t, "cert-lost") == nil {
			return errString("no pod yet")
		}
		return nil
	})
}

// A wake whose Certificate is not Ready keeps the Agent Resuming until the
// Certificate is Ready, then creates the Pod.
func TestAgent_WakeWaitsForCertificate(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-wake", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	provisionRunningAgentWithLifecycle(t, "cert-wake", "wc-cert-wake", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
		ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Second}
	})
	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "cert-wake", 2*time.Hour)}, 1)
	touchAgent(t, "cert-wake")
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, listAgentPods("cert-wake")...); err != nil {
			return err
		}
		for i := range pods.Items {
			if !pods.Items[i].DeletionTimestamp.IsZero() {
				forceDeletePod(t, &pods.Items[i])
			}
		}
		if ph := getWorkloadAgent(t, "cert-wake").Status.Phase; ph != kaalmv1beta1.AgentHibernated {
			return fmt.Errorf("phase=%s want Hibernated", ph)
		}
		return nil
	})

	// A Hibernated pass ends before the Certificate step, so the
	// Certificate is re-created by the wake's pass.
	var cert cmapi.Certificate
	key := types.NamespacedName{Namespace: "default", Name: agentCertificateName("cert-wake")}
	if err := testClient.Get(ctxT(), key, &cert); err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	if err := testClient.Delete(ctxT(), &cert); err != nil {
		t.Fatalf("delete certificate: %v", err)
	}
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "cert-wake")
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations[kaalmv1beta1.AnnotationWake] = kaalmv1beta1.AnnotationTrue
		return testClient.Update(ctxT(), ag)
	})
	expectAgentPhase(t, "cert-wake", kaalmv1beta1.AgentResuming)
	expectAgentReadyReason(t, "cert-wake", kaalmv1beta1.ReasonCertificateNotReady)
	consistently(t, holdWindow, func() error {
		if p := agentPod(t, "cert-wake"); p != nil {
			return fmt.Errorf("pod %s created before the certificate is Ready", p.Name)
		}
		if ph := getWorkloadAgent(t, "cert-wake").Status.Phase; ph != kaalmv1beta1.AgentResuming {
			return fmt.Errorf("phase=%s want Resuming", ph)
		}
		return nil
	})

	markCertReady(t, "cert-wake")
	eventually(t, func() error {
		if agentPod(t, "cert-wake") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	if ph := getWorkloadAgent(t, "cert-wake").Status.Phase; ph != kaalmv1beta1.AgentResuming {
		t.Errorf("phase=%s want Resuming until the Pod is Ready", ph)
	}
	// The hold outlasted the 1s idleTimeout: fresh traffic keeps the woken
	// Agent Running long enough to observe.
	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "cert-wake", 0)}, 1)
	markPodReady(t, agentPod(t, "cert-wake"))
	expectAgentPhase(t, "cert-wake", kaalmv1beta1.AgentRunning)
}

// mkHeldPod creates the Agent's Pod mounting tlsSecret, Ready unless status
// changes it.
func mkHeldPod(t *testing.T, c client.Client, agent *kaalmv1beta1.Agent, tlsSecret string,
	status func(*corev1.Pod)) {
	t.Helper()
	pod := desiredPod(agent, tlsDriftEff, "kaalm-system", tlsSecret)
	if err := controllerutil.SetControllerReference(agent, pod, c.Scheme()); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if status != nil {
		status(pod)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func podNotReady(p *corev1.Pod) {
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
}

func readyOf(agent *kaalmv1beta1.Agent) *metav1.Condition {
	return apimeta.FindStatusCondition(agent.Status.Conditions, kaalmv1beta1.ConditionReady)
}

func expectHeldReady(t *testing.T, agent *kaalmv1beta1.Agent, serving bool) {
	t.Helper()
	c := readyOf(agent)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonCertificateNotReady {
		t.Fatalf("Ready = %+v, want False CertificateNotReady", c)
	}
	if got := strings.Contains(c.Message, "keeps serving"); got != serving {
		t.Errorf("Ready message %q: says the Pod keeps serving = %v, want %v", c.Message, got, serving)
	}
}

// A drift found while the Certificate is held keeps the Pod, takes no slot,
// and says why it waits.
func TestConvergePod_CertHeldKeepsDriftedPodWithoutSlot(t *testing.T) {
	agent := tlsDriftAgent()
	c := slotClient(t, agent)
	mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls", nil)
	rec := record.NewFakeRecorder(10)
	r := &AgentReconciler{Client: c, Recorder: rec, OperatorNamespace: "kaalm-system"}

	waiting, rejected, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true)
	if err != nil || waiting || rejected {
		t.Fatalf("convergePod = (%v, %v, %v), want (false, false, nil)", waiting, rejected, err)
	}
	if pods := ownedPodsOf(t, c, agent); len(pods) != 1 || !pods[0].DeletionTimestamp.IsZero() {
		t.Fatalf("the held Agent's Pod was not kept: %d Pods", len(pods))
	}
	if len(r.driftSlots.reserved) != 0 {
		t.Errorf("drift slot reserved for a held Agent: %v", r.driftSlots.reserved)
	}
	cond := podUpToDate(agent)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != kaalmv1beta1.ReasonCertificateNotReady ||
		!strings.Contains(cond.Message, "legacy-tls-0f1e2d3c") || !strings.Contains(cond.Message, "mounts legacy-tls;") {
		t.Errorf("PodUpToDate = %+v, want False CertificateNotReady naming both Secrets", cond)
	}
	if agent.Status.Phase != kaalmv1beta1.AgentRunning {
		t.Errorf("phase = %s, want Running", agent.Status.Phase)
	}
	expectHeldReady(t, agent, true)
	for len(rec.Events) > 0 {
		if ev := <-rec.Events; strings.Contains(ev, "SpecDrift") {
			t.Errorf("unexpected event %q", ev)
		}
	}
	for _, ev := range r.events.take(agent) {
		if ev.reason == kaalmv1beta1.ReasonSpecDriftPending {
			t.Errorf("unexpected held event %+v", ev)
		}
	}
}

// A Replacing Agent whose drifted Pod serves gives its slot back while the
// Certificate is held.
func TestConvergePod_CertHeldReleasesSlotOfServingPod(t *testing.T) {
	agent := tlsDriftAgent()
	r := &AgentReconciler{OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(10)}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionPodUpToDate, Status: metav1.ConditionFalse, Reason: kaalmv1beta1.ReasonReplacing,
	})
	r.Client = slotClient(t, agent)
	mkHeldPod(t, r.Client, agent, "legacy-tls", nil)

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	if got := podUpToDateReason(agent); got != kaalmv1beta1.ReasonCertificateNotReady {
		t.Errorf("PodUpToDate reason %q, want CertificateNotReady", got)
	}
	if pods := ownedPodsOf(t, r.Client, agent); len(pods) != 1 {
		t.Fatalf("want the Pod kept, got %d Pods", len(pods))
	}
}

// A Replacing Agent whose Pod is not Ready keeps its slot while the
// Certificate is held, because the slot counts that unavailability.
func TestConvergePod_CertHeldKeepsSlotOfUnreadyPod(t *testing.T) {
	agent := tlsDriftAgent()
	r := &AgentReconciler{OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(10)}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionPodUpToDate, Status: metav1.ConditionFalse, Reason: kaalmv1beta1.ReasonReplacing,
	})
	r.Client = slotClient(t, agent)
	mkHeldPod(t, r.Client, agent, "legacy-tls", podNotReady)

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	if got := podUpToDateReason(agent); got != kaalmv1beta1.ReasonReplacing {
		t.Errorf("PodUpToDate reason %q, want Replacing", got)
	}
	if pods := ownedPodsOf(t, r.Client, agent); len(pods) != 1 || !pods[0].DeletionTimestamp.IsZero() {
		t.Fatalf("want the Pod kept, got %d Pods", len(pods))
	}
	expectHeldReady(t, agent, false)
}

// With no Pod, a held pass creates none and the Agent waits in Provisioning.
func TestConvergePod_CertHeldNoPod(t *testing.T) {
	agent := tlsDriftAgent()
	agent.Status.PodName = "legacy"
	c := slotClient(t, agent)
	r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	if pods := ownedPodsOf(t, c, agent); len(pods) != 0 {
		t.Fatalf("a held pass created %d Pods", len(pods))
	}
	if agent.Status.Phase != kaalmv1beta1.AgentProvisioning || agent.Status.PodName != "" {
		t.Errorf("phase=%s podName=%q, want Provisioning and no podName", agent.Status.Phase, agent.Status.PodName)
	}
	expectHeldReady(t, agent, false)
}

// A held pass derives the phase from the Pod as a normal pass does.
func TestConvergePod_CertHeldPhaseFromPod(t *testing.T) {
	crashLoop := func(p *corev1.Pod) {
		podNotReady(p)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "agent", RestartCount: crashLoopThreshold,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}
	}
	cases := []struct {
		name      string
		phase     kaalmv1beta1.AgentPhase
		status    func(*corev1.Pod)
		want      kaalmv1beta1.AgentPhase
		serving   bool
		wantPods  int
		terminate bool
	}{
		{"idle stays idle", kaalmv1beta1.AgentIdle, nil, kaalmv1beta1.AgentIdle, true, 1, false},
		{"resuming waits", kaalmv1beta1.AgentResuming, podNotReady, kaalmv1beta1.AgentResuming, false, 1, false},
		{"crash loop fails", kaalmv1beta1.AgentRunning, crashLoop, kaalmv1beta1.AgentFailed, false, 1, false},
		{"terminal pod removed", kaalmv1beta1.AgentRunning, func(p *corev1.Pod) {
			podNotReady(p)
			p.Status.Phase = corev1.PodFailed
		}, kaalmv1beta1.AgentProvisioning, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := tlsDriftAgent()
			agent.Status.Phase = tc.phase
			c := slotClient(t, agent)
			mkHeldPod(t, c, agent, "legacy-tls-0f1e2d3c", tc.status)
			r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

			if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
				"legacy-tls-0f1e2d3c", true); err != nil {
				t.Fatal(err)
			}
			if agent.Status.Phase != tc.want {
				t.Errorf("phase = %s, want %s", agent.Status.Phase, tc.want)
			}
			expectHeldReady(t, agent, tc.serving)
			if pods := ownedPodsOf(t, c, agent); len(pods) != tc.wantPods {
				t.Errorf("got %d Pods, want %d", len(pods), tc.wantPods)
			}
		})
	}
}

// Ready is written once per held pass with a stable status, so a settled
// hold changes no status and writes nothing.
func TestConvergePod_CertHeldReadyIsStable(t *testing.T) {
	agent := tlsDriftAgent()
	c := slotClient(t, agent)
	mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls-0f1e2d3c", nil)
	r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	first := agent.Status.DeepCopy()
	time.Sleep(1100 * time.Millisecond) // a flip would land on a new second
	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(*first, agent.Status) {
		t.Errorf("status changed on a settled held pass:\nfirst  %+v\nsecond %+v", *first, agent.Status)
	}
	if podUpToDateReason(agent) != kaalmv1beta1.ReasonPodCurrent {
		t.Errorf("PodUpToDate reason %q, want Current", podUpToDateReason(agent))
	}
}

// While the Certificate is held, an Idle Agent past its hibernation delay
// stays Idle; without the hold it hibernates.
func TestEvaluateActivity_HibernationHeld(t *testing.T) {
	eff := effectiveAgentSpec{IdleTimeout: time.Second, HibernationDelay: time.Second, HibernationEnabled: true}
	r := &AgentReconciler{Activity: &stubActivity{
		reachable: []ReplicaActivity{replicaWith(3*time.Hour, "idle", 2*time.Hour)}, total: 1,
	}}
	idle := func() *kaalmv1beta1.Agent {
		since := metav1.NewTime(time.Now().Add(-2 * time.Hour))
		return &kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: "default"},
			Status: kaalmv1beta1.AgentStatus{
				Phase: kaalmv1beta1.AgentIdle, PhaseTransitionTime: &since, LastActivityTime: &since,
			},
		}
	}

	held := idle()
	if res := r.evaluateActivity(context.Background(), held, eff, true); res.RequeueAfter != activityCacheWindow {
		t.Errorf("held: result %+v, want RequeueAfter %s", res, activityCacheWindow)
	}
	if held.Status.Phase != kaalmv1beta1.AgentIdle {
		t.Errorf("held: phase = %s, want Idle", held.Status.Phase)
	}

	free := idle()
	r.evaluateActivity(context.Background(), free, eff, false)
	if free.Status.Phase != kaalmv1beta1.AgentHibernating {
		t.Errorf("not held: phase = %s, want Hibernating", free.Status.Phase)
	}
}
