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

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const tlsDriftUID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// tlsDriftAgent is a Running Agent of the "slots" class with a fixed UID, so
// its UID-suffixed Secret name is legacy-tls-0f1e2d3c.
func tlsDriftAgent() *kaalmv1beta1.Agent {
	ag := slotAgent("legacy", kaalmv1beta1.AgentRunning, "")
	ag.UID = tlsDriftUID
	return ag
}

// mkReadyPod creates the Agent's Ready Pod mounting the given TLS Secret.
func mkReadyPod(t *testing.T, c client.Client, agent *kaalmv1beta1.Agent, eff effectiveAgentSpec,
	tlsSecret string, mutate func(*corev1.Pod)) {
	t.Helper()
	pod := desiredPod(agent, eff, "kaalm-system", tlsSecret)
	if err := controllerutil.SetControllerReference(agent, pod, c.Scheme()); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(pod)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func ownedPodsOf(t *testing.T, c client.Client, agent *kaalmv1beta1.Agent) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(agent.Namespace),
		client.MatchingLabels(agentPodLabels(agent))); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

func podUpToDate(agent *kaalmv1beta1.Agent) *metav1.Condition {
	return apimeta.FindStatusCondition(agent.Status.Conditions, kaalmv1beta1.ConditionPodUpToDate)
}

var tlsDriftEff = effectiveAgentSpec{Image: "img:v1", HealthPort: 8080, ServicePort: 8080}

// A Pod that mounts a Secret other than the one its Certificate names is
// replaced through the drift path, and the new Pod mounts the Certificate's
// Secret.
func TestConvergePod_TLSSecretMismatchReplacesPod(t *testing.T) {
	ctx := context.Background()
	agent := tlsDriftAgent()
	c := slotClient(t, agent)
	mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls", nil)
	rec := record.NewFakeRecorder(10)
	r := &AgentReconciler{Client: c, Recorder: rec, OperatorNamespace: "kaalm-system"}

	waiting, rejected, err := r.convergePod(ctx, agent, slotClass(nil), tlsDriftEff, "legacy-tls-0f1e2d3c", false)
	if err != nil || waiting || rejected {
		t.Fatalf("convergePod = (%v, %v, %v), want (false, false, nil)", waiting, rejected, err)
	}
	if pods := ownedPodsOf(t, c, agent); len(pods) != 0 {
		t.Fatalf("Pod mounting the old Secret was kept: %d Pods", len(pods))
	}
	if cond := podUpToDate(agent); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != kaalmv1beta1.ReasonReplacing {
		t.Errorf("PodUpToDate = %+v, want False Replacing", cond)
	}
	if agent.Status.Phase != kaalmv1beta1.AgentProvisioning {
		t.Errorf("phase = %s, want Provisioning", agent.Status.Phase)
	}
	var events []string
	for len(rec.Events) > 0 {
		events = append(events, <-rec.Events)
	}
	found := false
	for _, ev := range events {
		if strings.HasPrefix(ev, "Normal SpecDrift ") &&
			strings.Contains(ev, "legacy-tls-0f1e2d3c") && strings.Contains(ev, "mounts legacy-tls;") {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %q, want a Normal SpecDrift naming both Secrets", events)
	}

	if _, _, err := r.convergePod(ctx, agent, slotClass(nil), tlsDriftEff, "legacy-tls-0f1e2d3c", false); err != nil {
		t.Fatalf("second convergePod: %v", err)
	}
	pods := ownedPodsOf(t, c, agent)
	if len(pods) != 1 {
		t.Fatalf("want 1 replacement Pod, got %d", len(pods))
	}
	if got := tlsSecretOf(&pods[0]); got != "legacy-tls-0f1e2d3c" {
		t.Errorf("replacement Pod mounts %q, want legacy-tls-0f1e2d3c", got)
	}
}

// With no drift slot free, a TLS Secret mismatch waits like spec drift: the
// Pod keeps serving and PodUpToDate says why it waits.
func TestConvergePod_TLSSecretMismatchWaitsForSlot(t *testing.T) {
	ctx := context.Background()
	agent := tlsDriftAgent()
	holder := slotAgent("holder", kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonReplacing)
	c := slotClient(t, agent, holder)
	mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls", nil)
	r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

	class := slotClass(ptrIntOrString(intstr.FromInt32(1)))
	waiting, _, err := r.convergePod(ctx, agent, class, tlsDriftEff, "legacy-tls-0f1e2d3c", false)
	if err != nil || !waiting {
		t.Fatalf("convergePod = (waiting %v, err %v), want (true, nil)", waiting, err)
	}
	pods := ownedPodsOf(t, c, agent)
	if len(pods) != 1 || tlsSecretOf(&pods[0]) != "legacy-tls" {
		t.Fatalf("the waiting Agent's Pod was not kept: %d Pods", len(pods))
	}
	cond := podUpToDate(agent)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != kaalmv1beta1.ReasonReplacementPending ||
		!strings.Contains(cond.Message, "legacy-tls-0f1e2d3c") {
		t.Errorf("PodUpToDate = %+v, want False ReplacementPending naming the Secrets", cond)
	}
	if agent.Status.Phase != kaalmv1beta1.AgentRunning {
		t.Errorf("phase = %s, want Running", agent.Status.Phase)
	}
}

// A Pod that mounts the Certificate's Secret, or has no TLS volume to
// compare, is kept: an upgrade that keeps a Certificate's name replaces no
// Pod.
func TestConvergePod_TLSSecretMatchKeepsPod(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*corev1.Pod)
		wantMsg string
	}{
		{"same Secret", nil, "agent Pod matches the derived spec and mounts the Certificate's TLS Secret legacy-tls"},
		{"no TLS volume", func(p *corev1.Pod) {
			vols := p.Spec.Volumes[:0]
			for _, v := range p.Spec.Volumes {
				if v.Name != tlsVolumeName {
					vols = append(vols, v)
				}
			}
			p.Spec.Volumes = vols
			for i := range p.Spec.Containers {
				mounts := p.Spec.Containers[i].VolumeMounts[:0]
				for _, m := range p.Spec.Containers[i].VolumeMounts {
					if m.Name != tlsVolumeName {
						mounts = append(mounts, m)
					}
				}
				p.Spec.Containers[i].VolumeMounts = mounts
			}
		}, "agent Pod matches the derived spec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := tlsDriftAgent()
			c := slotClient(t, agent)
			mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls", tc.mutate)
			r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

			waiting, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff, "legacy-tls", false)
			if err != nil || waiting {
				t.Fatalf("convergePod = (waiting %v, err %v), want (false, nil)", waiting, err)
			}
			if pods := ownedPodsOf(t, c, agent); len(pods) != 1 {
				t.Fatalf("want the Pod kept, got %d Pods", len(pods))
			}
			if cond := podUpToDate(agent); cond == nil || cond.Status != metav1.ConditionTrue ||
				cond.Reason != kaalmv1beta1.ReasonPodCurrent || cond.Message != tc.wantMsg {
				t.Errorf("PodUpToDate = %+v, want True Current with message %q", cond, tc.wantMsg)
			}
		})
	}
}

// Changing the Certificate's spec.secretName drives a pass through the
// Certificate watch, and the Agent's Pod is replaced to mount the new name.
// The controller never updates an existing Certificate and envtest has no
// cert-manager, so the edit stands in for a Certificate re-created under
// another name.
func TestAgent_CertificateSecretNameChangeReplacesPod(t *testing.T) {
	mkWorkloadClass(t, "wc-tls-drift", nil)
	old := provisionRunningAgent(t, "tls-drift", "wc-tls-drift")
	expectDriftReason(t, "tls-drift", kaalmv1beta1.ReasonPodCurrent)
	if got := tlsSecretOf(old); got == "tls-drift-tls" {
		t.Fatalf("new Pod already mounts %q", got)
	}

	eventually(t, func() error {
		var cert cmapi.Certificate
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "tls-drift-tls"}, &cert); err != nil {
			return err
		}
		cert.Spec.SecretName = "tls-drift-tls"
		return testClient.Update(ctxT(), &cert)
	})

	eventually(t, func() error {
		var got corev1.Pod
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: old.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if got.UID != old.UID {
			return nil
		}
		if got.DeletionTimestamp.IsZero() {
			return errString("old pod not yet marked for deletion")
		}
		forceDeletePod(t, &got)
		return nil
	})
	var fresh *corev1.Pod
	eventually(t, func() error {
		fresh = agentPod(t, "tls-drift")
		if fresh == nil || fresh.UID == old.UID {
			return errString("no replacement pod yet")
		}
		if got := tlsSecretOf(fresh); got != "tls-drift-tls" {
			return fmt.Errorf("replacement Pod mounts %q, want tls-drift-tls", got)
		}
		return nil
	})
	markPodReady(t, fresh)
	expectDriftReason(t, "tls-drift", kaalmv1beta1.ReasonPodCurrent)
	expectAgentPhase(t, "tls-drift", kaalmv1beta1.AgentRunning)
}
