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
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// mkWorkloadClass creates an AgentClass that admits the test image, with
// optional extra mutation.
func mkWorkloadClass(t *testing.T, name string, mutate func(*kaalmv1beta1.AgentClass)) {
	t.Helper()
	ac := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kaalmv1beta1.AgentClassSpec{
			Image: kaalmv1beta1.AgentClassImage{AllowedImages: []string{"registry.test/agents/*"}},
		},
	}
	if mutate != nil {
		mutate(ac)
	}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
}

func mkWorkloadAgent(t *testing.T, name, className string, mutate func(*kaalmv1beta1.Agent)) {
	t.Helper()
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: className},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if mutate != nil {
		mutate(ag)
	}
	if err := testClient.Create(ctxT(), ag); err != nil {
		t.Fatalf("create agent %s: %v", name, err)
	}
}

func getWorkloadAgent(t *testing.T, name string) *kaalmv1beta1.Agent {
	t.Helper()
	var ag kaalmv1beta1.Agent
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag); err != nil {
		t.Fatalf("get agent %s: %v", name, err)
	}
	return &ag
}

// markCertReady plays cert-manager: it waits for the Certificate the
// reconciler creates and flips its Ready condition to True.
func markCertReady(t *testing.T, agentName string) {
	t.Helper()
	eventually(t, func() error { return markCertReadyErr(agentName) })
}

// markCertReadyErr is the error-returning core, shared with the task suite.
func markCertReadyErr(workloadName string) error {
	var cert cmapi.Certificate
	key := types.NamespacedName{Namespace: "default", Name: workloadName + "-tls"}
	if err := testClient.Get(ctxT(), key, &cert); err != nil {
		return err
	}
	cert.Status.Conditions = []cmapi.CertificateCondition{{
		Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, Reason: "Issued",
	}}
	return testClient.Status().Update(ctxT(), &cert)
}

// agentPod fetches the Agent's current non-terminating Pod, or nil.
func agentPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := testClient.List(ctxT(), &pods, client.InNamespace("default"),
		client.MatchingLabels(map[string]string{"kaalm.io/agent": name})); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			return &pods.Items[i]
		}
	}
	return nil
}

// markPodReady simulates the kubelet bringing a Pod up.
func markPodReady(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("mark pod ready: %v", err)
	}
}

// forceDeletePod finishes a graceful deletion instantly (envtest has no
// kubelet to complete termination).
func forceDeletePod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	zero := int64(0)
	if err := testClient.Delete(ctxT(), pod, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil &&
		!apierrors.IsNotFound(err) {
		t.Fatalf("force delete pod: %v", err)
	}
}

func expectAgentPhase(t *testing.T, name string, phase kaalmv1beta1.AgentPhase) {
	t.Helper()
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag); err != nil {
			return err
		}
		if ag.Status.Phase != phase {
			return errString("phase=" + string(ag.Status.Phase) + " want " + string(phase))
		}
		return nil
	})
}

func expectAgentReadyReason(t *testing.T, name, reason string) {
	t.Helper()
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag); err != nil {
			return err
		}
		c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil {
			return errString("no Ready condition yet")
		}
		if c.Reason != reason {
			return errString("reason=" + c.Reason + " want " + reason)
		}
		return nil
	})
}

// ---- Happy path ----

func TestAgent_ProvisionToRunning(t *testing.T) {
	mkWorkloadClass(t, "wc-run", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
	})
	mkWorkloadAgent(t, "run-agent", "wc-run", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
	})

	// Certificate is created and gates the Pod.
	eventually(t, func() error {
		var cert cmapi.Certificate
		return testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "run-agent-tls"}, &cert)
	})
	if pod := agentPod(t, "run-agent"); pod != nil {
		t.Fatal("Pod created before the Certificate was Ready")
	}
	expectAgentReadyReason(t, "run-agent", "CertificateNotReady")

	markCertReady(t, "run-agent")

	// All children converge and the Pod appears.
	eventually(t, func() error {
		if pod := agentPod(t, "run-agent"); pod == nil {
			return errString("no pod yet")
		}
		return nil
	})
	for _, probe := range []struct {
		kind string
		get  func() error
	}{
		{"ServiceAccount", func() error {
			var sa corev1.ServiceAccount
			return testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "agent-run-agent"}, &sa)
		}},
		{"Service", func() error {
			var svc corev1.Service
			return testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "run-agent"}, &svc)
		}},
		{"PVC", func() error {
			var pvc corev1.PersistentVolumeClaim
			return testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "run-agent-memory"}, &pvc)
		}},
	} {
		if err := probe.get(); err != nil {
			t.Errorf("%s missing: %v", probe.kind, err)
		}
	}
	expectAgentPhase(t, "run-agent", kaalmv1beta1.AgentProvisioning)

	// Kubelet brings the Pod up; the Agent goes Running.
	markPodReady(t, agentPod(t, "run-agent"))
	expectAgentPhase(t, "run-agent", kaalmv1beta1.AgentRunning)
	ag := getWorkloadAgent(t, "run-agent")
	if ag.Status.Endpoint == "" || ag.Status.PodName == "" || ag.Status.PVCName != "run-agent-memory" {
		t.Errorf("status incomplete: endpoint=%q podName=%q pvcName=%q",
			ag.Status.Endpoint, ag.Status.PodName, ag.Status.PVCName)
	}
	c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != kaalmv1beta1.ReasonPodRunning {
		t.Errorf("Ready condition wrong: %+v", c)
	}
}

// ---- Gates and guards ----

func TestAgent_SystemNamespaceForbidden(t *testing.T) {
	mkWorkloadClass(t, "wc-sys", nil)
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sys-agent", Namespace: testSystemNamespace},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "wc-sys"},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), ag); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.Agent
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "sys-agent"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonSystemNamespaceForbidden {
			return errString("SystemNamespaceForbidden not set")
		}
		return nil
	})
	// No child resources may exist.
	var cert cmapi.Certificate
	err := testClient.Get(ctxT(), types.NamespacedName{Namespace: testSystemNamespace, Name: "sys-agent-tls"}, &cert)
	if !apierrors.IsNotFound(err) {
		t.Errorf("Certificate was created in the system namespace: %v", err)
	}
}

func TestAgent_MissingClass(t *testing.T) {
	mkWorkloadAgent(t, "noclass-agent", "does-not-exist", nil)
	expectAgentReadyReason(t, "noclass-agent", kaalmv1beta1.ReasonInvalidReference)
}

func TestAgent_ImagePullSecretMissing(t *testing.T) {
	mkWorkloadClass(t, "wc-pull", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "wc-pull-registry-creds"}}
	})
	mkWorkloadAgent(t, "pull-agent", "wc-pull", nil)
	expectAgentReadyReason(t, "pull-agent", kaalmv1beta1.ReasonImagePullSecretMissing)

	// Creating the Secret recovers the gate.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wc-pull-registry-creds", Namespace: "default"}}
	if err := testClient.Create(ctxT(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	expectAgentReadyReason(t, "pull-agent", "CertificateNotReady")
}

func TestAgent_ExistingClaimNotFound(t *testing.T) {
	mkWorkloadClass(t, "wc-claim", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
	})
	claim := "no-such-claim"
	mkWorkloadAgent(t, "claim-agent", "wc-claim", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Persistence.ExistingClaim = &claim
	})
	expectAgentReadyReason(t, "claim-agent", kaalmv1beta1.ReasonExistingClaimNotFound)
}

func TestAgent_HandlerConfigMapNotFoundGatesAndRecovers(t *testing.T) {
	mkWorkloadClass(t, "wc-hcm", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.AllowHandlerMounts = true
	})
	mkWorkloadAgent(t, "hcm-agent", "wc-hcm", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Handler = &kaalmv1beta1.AgentHandler{
			ConfigMapRef: kaalmv1beta1.LocalObjectReference{Name: "hcm-handler"},
		}
	})
	// Rule 31: a clear condition, not a Pod wedged on a missing volume source.
	expectAgentReadyReason(t, "hcm-agent", kaalmv1beta1.ReasonHandlerConfigMapNotFound)

	// Creating the ConfigMap recovers the gate.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "hcm-handler", Namespace: "default"},
		Data:       map[string]string{"handler.py": "def handle_message(envelope): ..."},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create handler configmap: %v", err)
	}
	expectAgentReadyReason(t, "hcm-agent", "CertificateNotReady")

	// The reconciler never takes ownership of the developer's ConfigMap.
	eventually(t, func() error {
		var got corev1.ConfigMap
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "hcm-handler"}, &got); err != nil {
			return err
		}
		if len(got.OwnerReferences) != 0 {
			return errString("handler ConfigMap must carry no ownerRef")
		}
		return nil
	})
}

// ---- Degraded paths ----

func TestAgent_ImageNotAllowedDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-img", nil)
	mkWorkloadAgent(t, "img-agent", "wc-img", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Image = "evil.example/agents/demo:v1"
	})
	expectAgentPhase(t, "img-agent", kaalmv1beta1.AgentDegraded)
	ag := getWorkloadAgent(t, "img-agent")
	if ag.Status.PreDegradedPhase != kaalmv1beta1.AgentPending {
		t.Errorf("preDegradedPhase = %q, want Pending", ag.Status.PreDegradedPhase)
	}
	expectAgentReadyReason(t, "img-agent", kaalmv1beta1.ReasonClassConstraintViolation)
}

func TestAgent_ProviderNamespaceDeniedDegrades(t *testing.T) {
	mkSecret(t, "prov-ns-key")
	mkProvider(t, "prov-ns", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "prov-ns-key", Key: "token"}
		mp.Spec.AllowedNamespaces = []string{"team-*"}
	})
	mkWorkloadClass(t, "wc-prov", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "prov-ns"}}
	})
	mkWorkloadAgent(t, "prov-agent", "wc-prov", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "prov-ns"}},
		}
	})
	// The agent's namespace (default) does not match team-*.
	expectAgentPhase(t, "prov-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "prov-agent", kaalmv1beta1.ReasonClassConstraintViolation)
}

func TestAgent_PersistenceNotAllowedDegradesAndRecovers(t *testing.T) {
	mkWorkloadClass(t, "wc-per", nil) // persistence disabled on the class
	mkWorkloadAgent(t, "per-agent", "wc-per", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
	})
	expectAgentPhase(t, "per-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "per-agent", kaalmv1beta1.ReasonPersistenceNotAllowed)

	// Platform team enables persistence on the class: the Agent recovers to
	// its pre-degradation phase and provisioning proceeds.
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "wc-per"}, &ac); err != nil {
			return err
		}
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		return testClient.Update(ctxT(), &ac)
	})
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "per-agent")
		if ag.Status.Phase == kaalmv1beta1.AgentDegraded {
			return errString("still Degraded")
		}
		if ag.Status.PreDegradedPhase != "" {
			return errString("preDegradedPhase not cleared")
		}
		return nil
	})
}

func TestAgent_HibernationRequiresPersistenceDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-hib", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	mkWorkloadAgent(t, "hib-agent", "wc-hib", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Lifecycle.HibernationEnabled = true // no persistence
	})
	expectAgentPhase(t, "hib-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "hib-agent", kaalmv1beta1.ReasonHibernationRequiresPersist)
}

func TestAgent_HibernationNotAllowedDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-hib2", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
	})
	mkWorkloadAgent(t, "hib2-agent", "wc-hib2", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true // class does not allow it
	})
	expectAgentPhase(t, "hib2-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "hib2-agent", kaalmv1beta1.ReasonHibernationNotAllowed)
}

func TestAgent_HandlerMountNotAllowedDegradesAndRecovers(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "hm-handler", Namespace: "default"},
		Data:       map[string]string{"handler.py": "def handle_message(envelope): ..."},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create handler configmap: %v", err)
	}
	mkWorkloadClass(t, "wc-hm", nil) // allowHandlerMounts defaults to false
	mkWorkloadAgent(t, "hm-agent", "wc-hm", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Handler = &kaalmv1beta1.AgentHandler{
			ConfigMapRef: kaalmv1beta1.LocalObjectReference{Name: "hm-handler"},
		}
	})
	// Rule 30: the class is the authority on ConfigMap-sourced code.
	expectAgentPhase(t, "hm-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "hm-agent", kaalmv1beta1.ReasonHandlerMountNotAllowed)

	// The platform team grants the capability: the Agent recovers, the same
	// class-drift path that degrades when the gate is flipped off.
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "wc-hm"}, &ac); err != nil {
			return err
		}
		ac.Spec.Image.AllowHandlerMounts = true
		return testClient.Update(ctxT(), &ac)
	})
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "hm-agent")
		if ag.Status.Phase == kaalmv1beta1.AgentDegraded {
			return errString("still Degraded")
		}
		if ag.Status.PreDegradedPhase != "" {
			return errString("preDegradedPhase not cleared")
		}
		return nil
	})
}

// ---- Drift and disruption ----

// provisionRunningAgent drives an agent to Running and returns its Pod.
func provisionRunningAgent(t *testing.T, name, className string) *corev1.Pod {
	t.Helper()
	mkWorkloadAgent(t, name, className, nil)
	markCertReady(t, name)
	eventually(t, func() error {
		if agentPod(t, name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := agentPod(t, name)
	markPodReady(t, pod)
	expectAgentPhase(t, name, kaalmv1beta1.AgentRunning)
	return agentPod(t, name)
}

func TestAgent_SpecDriftReplacesPod(t *testing.T) {
	mkWorkloadClass(t, "wc-drift", nil)
	oldPod := provisionRunningAgent(t, "drift-agent", "wc-drift")
	oldHash := oldPod.Annotations["kaalm.io/pod-spec-hash"]

	// A replacement-triggering edit: new env var.
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "drift-agent")
		ag.Spec.Env = []corev1.EnvVar{{Name: "NEW_FLAG", Value: "on"}}
		return testClient.Update(ctxT(), ag)
	})

	// The old Pod is deleted (deletionTimestamp set); envtest has no kubelet,
	// so finish the termination for it.
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
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
	expectAgentPhase(t, "drift-agent", kaalmv1beta1.AgentProvisioning)

	// A new Pod appears with a different hash.
	eventually(t, func() error {
		pod := agentPod(t, "drift-agent")
		if pod == nil {
			return errString("no replacement pod yet")
		}
		if pod.Name == oldPod.Name {
			return errString("old pod still current")
		}
		if pod.Annotations["kaalm.io/pod-spec-hash"] == oldHash {
			return errString("replacement pod has the old hash")
		}
		return nil
	})
}

func TestAgent_HandlerRepointReplacesPod(t *testing.T) {
	for _, name := range []string{"greeter-v1", "greeter-v2"} {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Data:       map[string]string{"handler.py": "def handle_message(envelope): ..."},
		}
		if err := testClient.Create(ctxT(), cm); err != nil {
			t.Fatalf("create handler configmap %s: %v", name, err)
		}
	}
	mkWorkloadClass(t, "wc-repoint", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.AllowHandlerMounts = true
	})
	mkWorkloadAgent(t, "repoint-agent", "wc-repoint", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Handler = &kaalmv1beta1.AgentHandler{
			ConfigMapRef: kaalmv1beta1.LocalObjectReference{Name: "greeter-v1"},
		}
	})
	markCertReady(t, "repoint-agent")
	eventually(t, func() error {
		if agentPod(t, "repoint-agent") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	oldPod := agentPod(t, "repoint-agent")
	markPodReady(t, oldPod)
	expectAgentPhase(t, "repoint-agent", kaalmv1beta1.AgentRunning)
	oldHash := oldPod.Annotations["kaalm.io/pod-spec-hash"]

	// Repointing the reference is ordinary Pod-replacing spec drift: the
	// versioned-ConfigMap rollout pattern from the design book.
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "repoint-agent")
		ag.Spec.Handler.ConfigMapRef.Name = "greeter-v2"
		return testClient.Update(ctxT(), ag)
	})
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
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
		pod := agentPod(t, "repoint-agent")
		if pod == nil {
			return errString("no replacement pod yet")
		}
		if pod.Name == oldPod.Name || pod.Annotations["kaalm.io/pod-spec-hash"] == oldHash {
			return errString("pod not replaced by the repoint")
		}
		for _, v := range pod.Spec.Volumes {
			if v.ConfigMap != nil && v.ConfigMap.Name == "greeter-v2" {
				return nil
			}
		}
		return errString("replacement pod does not mount greeter-v2")
	})
}

func TestAgent_InvoluntaryDisruptionReprovisions(t *testing.T) {
	mkWorkloadClass(t, "wc-disrupt", nil)
	pod := provisionRunningAgent(t, "disrupt-agent", "wc-disrupt")

	// Out-of-band force delete (node loss / manual kubectl delete).
	forceDeletePod(t, pod)

	eventually(t, func() error {
		newPod := agentPod(t, "disrupt-agent")
		if newPod == nil {
			return errString("no replacement pod yet")
		}
		if newPod.Name == pod.Name {
			return errString("old pod still present")
		}
		return nil
	})
}

// ---- Finalizer ----

func TestAgent_FinalizerRetainStripsPVCOwnerRef(t *testing.T) {
	mkWorkloadClass(t, "wc-retain", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Persistence.PVCRetention = "Retain"
	})
	mkWorkloadAgent(t, "retain-agent", "wc-retain", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
	})
	markCertReady(t, "retain-agent")
	eventually(t, func() error {
		if agentPod(t, "retain-agent") == nil {
			return errString("no pod yet")
		}
		return nil
	})

	ag := getWorkloadAgent(t, "retain-agent")
	if err := testClient.Delete(ctxT(), ag); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	// Finish the Pod's graceful termination for the kubelet-less envtest.
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, client.InNamespace("default"),
			client.MatchingLabels(map[string]string{"kaalm.io/agent": "retain-agent"})); err != nil {
			return err
		}
		for i := range pods.Items {
			forceDeletePod(t, &pods.Items[i])
		}
		return nil
	})

	// The Agent finalizes away and the PVC survives with no Agent ownerRef.
	eventually(t, func() error {
		var got kaalmv1beta1.Agent
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "retain-agent"}, &got)
		if !apierrors.IsNotFound(err) {
			return errString("agent not yet finalized")
		}
		return nil
	})
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "retain-agent-memory"}, &pvc); err != nil {
		t.Fatalf("PVC should survive under Retain: %v", err)
	}
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == "Agent" {
			t.Errorf("PVC still carries an Agent ownerRef under Retain: %+v", ref)
		}
	}
}

func TestAgent_FinalizerDeleteKeepsPVCOwnerRef(t *testing.T) {
	mkWorkloadClass(t, "wc-delete", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		// PVCRetention defaults to Delete.
	})
	mkWorkloadAgent(t, "delete-agent", "wc-delete", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
	})
	markCertReady(t, "delete-agent")
	eventually(t, func() error {
		if agentPod(t, "delete-agent") == nil {
			return errString("no pod yet")
		}
		return nil
	})

	ag := getWorkloadAgent(t, "delete-agent")
	if err := testClient.Delete(ctxT(), ag); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, client.InNamespace("default"),
			client.MatchingLabels(map[string]string{"kaalm.io/agent": "delete-agent"})); err != nil {
			return err
		}
		for i := range pods.Items {
			forceDeletePod(t, &pods.Items[i])
		}
		return nil
	})
	eventually(t, func() error {
		var got kaalmv1beta1.Agent
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "delete-agent"}, &got)
		if !apierrors.IsNotFound(err) {
			return errString("agent not yet finalized")
		}
		return nil
	})
	// envtest runs no garbage collector, so the PVC is still present; the
	// contract under Delete is that its Agent ownerRef stays intact so
	// cascade GC would remove it in a real cluster.
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "delete-agent-memory"}, &pvc); err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	found := false
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == "Agent" {
			found = true
		}
	}
	if !found {
		t.Error("PVC lost its Agent ownerRef under Delete retention")
	}
}

// ---- Agent convergePod: crash loop -> Failed ----

func TestAgent_CrashLoopMarksFailed(t *testing.T) {
	mkWorkloadClass(t, "wc-crash", nil)
	pod := provisionRunningAgent(t, "crash-agent", "wc-crash")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "agent",
		RestartCount: crashLoopThreshold,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: "back-off restarting",
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectAgentPhase(t, "crash-agent", kaalmv1beta1.AgentFailed)
	expectAgentReadyReason(t, "crash-agent", "CrashLoopBackOff")
}

// ---- Agent convergePod: terminal Pod is re-provisioned ----

func TestAgent_TerminalPodReprovisions(t *testing.T) {
	mkWorkloadClass(t, "wc-term", nil)
	pod := provisionRunningAgent(t, "term-agent", "wc-term")
	pod.Status.Phase = corev1.PodFailed
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	// The reconciler deletes the terminal Pod; finish its termination.
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !got.DeletionTimestamp.IsZero() {
			forceDeletePod(t, &got)
		}
		return errString("terminal pod still present")
	})
	// A fresh Pod is provisioned.
	eventually(t, func() error {
		newPod := agentPod(t, "term-agent")
		if newPod == nil || newPod.Name == pod.Name {
			return errString("no replacement pod yet")
		}
		return nil
	})
}

// ---- Agent degradedReasons: provider named in the class but the CR is absent ----

func TestAgent_ProviderMissingDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-provmiss", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "ghost-prov"}}
	})
	mkWorkloadAgent(t, "provmiss-agent", "wc-provmiss", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "ghost-prov"}},
		}
	})
	expectAgentPhase(t, "provmiss-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "provmiss-agent", kaalmv1beta1.ReasonClassConstraintViolation)
}

// ---- Agent: a provider outside the class allowlist degrades ----

func TestAgent_ProviderNotAllowedDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-provdenied", nil) // empty allowedProviders => none allowed
	mkWorkloadAgent(t, "provdenied-agent", "wc-provdenied", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "some-prov"}},
		}
	})
	expectAgentPhase(t, "provdenied-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "provdenied-agent", kaalmv1beta1.ReasonClassConstraintViolation)
}

// ---- Agent convergePod: a terminating Pod holds the agent in Provisioning ----

func TestAgent_TerminatingPodHoldsProvisioning(t *testing.T) {
	mkWorkloadClass(t, "wc-terming", nil)
	pod := provisionRunningAgent(t, "terming-agent", "wc-terming")

	// A finalizer keeps the Pod present with a deletionTimestamp set (envtest
	// removes unscheduled Pods instantly otherwise), so convergePod observes a
	// replacement in progress and holds the agent in Provisioning.
	const fin = "test.kaalm.io/hold"
	eventually(t, func() error {
		var got corev1.Pod
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got); err != nil {
			return err
		}
		got.Finalizers = append(got.Finalizers, fin)
		return testClient.Update(ctxT(), &got)
	})
	if err := testClient.Delete(ctxT(), pod); err != nil {
		t.Fatalf("graceful delete: %v", err)
	}
	touchAgent(t, "terming-agent")
	expectAgentPhase(t, "terming-agent", kaalmv1beta1.AgentProvisioning)
	expectAgentReadyReason(t, "terming-agent", "PodProvisioning")

	// Release the finalizer so the Pod can be reaped.
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		got.Finalizers = nil
		return testClient.Update(ctxT(), &got)
	})
}

// ---- Agent ensureService: a drifted Service is reconciled back ----

func TestAgent_ServicePortDriftReconciled(t *testing.T) {
	mkWorkloadClass(t, "wc-svcdrift", nil)
	provisionRunningAgent(t, "svcdrift-agent", "wc-svcdrift")

	var svc corev1.Service
	key := types.NamespacedName{Namespace: "default", Name: "svcdrift-agent"}
	eventually(t, func() error { return testClient.Get(ctxT(), key, &svc) })
	wantPort := svc.Spec.Ports[0].Port

	// Drift the Service port out of band.
	svc.Spec.Ports[0].Port = 9999
	if err := testClient.Update(ctxT(), &svc); err != nil {
		t.Fatalf("drift service: %v", err)
	}
	touchAgent(t, "svcdrift-agent")

	eventually(t, func() error {
		var got corev1.Service
		if err := testClient.Get(ctxT(), key, &got); err != nil {
			return err
		}
		if got.Spec.Ports[0].Port != wantPort {
			return errString("service port not reconciled back")
		}
		return nil
	})
}

func TestAgentReconciler_NowUsesClock(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := &AgentReconciler{Clock: func() time.Time { return fixed }}
	if got := r.now(); !got.Equal(fixed) {
		t.Errorf("now() = %v, want injected clock %v", got, fixed)
	}
	// A nil clock falls back to wall time (just prove it returns something recent).
	def := &AgentReconciler{}
	if def.now().IsZero() {
		t.Error("default now() must return wall time")
	}
}

func TestEnsureChildren_CreateErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	c := newErrCreateClient(t)

	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
	class := &kaalmv1beta1.AgentClass{}
	ar := &AgentReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	eff := effectiveAgentSpec{HealthPort: 8080, ServicePort: 8080, ServiceEnabled: true, PersistenceOn: true, PVCSizeGi: 1}

	if err := ar.ensureServiceAccount(ctx, agent); err == nil {
		t.Error("ensureServiceAccount must surface a create error")
	}
	if err := ar.ensureService(ctx, agent, eff); err == nil {
		t.Error("ensureService must surface a create error")
	}
	if err := ar.ensurePVC(ctx, agent, class, eff); err == nil {
		t.Error("ensurePVC must surface a create error")
	}
	if err := ar.ensureNetworkPolicy(ctx, agent, class, eff); err == nil {
		t.Error("ensureNetworkPolicy must surface a create error")
	}
	// The Certificate does not exist yet, so ensureCertificate takes the create
	// path and surfaces the failure.
	if _, err := ar.ensureCertificate(ctx, agent); err == nil {
		t.Error("ensureCertificate must surface a create error")
	}
	// convergePod finds no Pod and fails to create one.
	if err := ar.convergePod(ctx, agent, eff); err == nil {
		t.Error("convergePod must surface a create error")
	}

	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"}}
	tr := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	if err := tr.ensureTaskChildren(ctx, task, class, effectiveTaskSpec{PersistenceOn: true, PVCSizeGi: 1}); err == nil {
		t.Error("ensureTaskChildren must surface a create error")
	}
	if _, err := tr.ensureTaskCertificate(ctx, task); err == nil {
		t.Error("ensureTaskCertificate must surface a create error")
	}
}
