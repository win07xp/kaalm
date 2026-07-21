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
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- ModelProvider delete handshake (reconcileDelete + isReferenced) ----

func TestModelProvider_DeleteHeldWhileReferenced(t *testing.T) {
	mkSecret(t, "mpref-key")
	mkProvider(t, "mpref", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mpref-key", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)

	// An AgentClass listing the provider makes isReferenced true (classes branch).
	mkClass(t, "acref", "mpref")

	// Delete the provider: the finalizer holds it in Terminating while referenced.
	var mp agentryv1alpha1.ModelProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &mp); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp); err != nil {
		t.Fatalf("provider removed while still referenced: %v", err)
	}

	// Remove the referrer; the provider finalizes away.
	var ac agentryv1alpha1.AgentClass
	_ = testClient.Get(ctxT(), types.NamespacedName{Name: "acref"}, &ac)
	if err := testClient.Delete(ctxT(), &ac); err != nil {
		t.Fatalf("delete class: %v", err)
	}
	eventually(t, func() error {
		var got agentryv1alpha1.ModelProvider
		err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("provider not yet finalized")
	})
}

// ---- ModelProvider probe outcomes (Skipped, Err, interval) ----

func TestModelProvider_ProbeSkipped(t *testing.T) {
	mkSecret(t, "mp-skip-key")
	fakeHealth.set("mp-skip", ProviderProbeResult{Skipped: true})
	mkProvider(t, "mp-skip", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-skip-key", Key: "token"}
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	get := func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-skip"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), agentryv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "ProbeSkipped" {
			return errString("Healthy should be Unknown/ProbeSkipped")
		}
		return nil
	})
}

func TestModelProvider_ProbeErrStaysReady(t *testing.T) {
	mkSecret(t, "mp-unhealthy-key")
	fakeHealth.set("mp-unhealthy", ProviderProbeResult{Err: errString("upstream 500")})
	mkProvider(t, "mp-unhealthy", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-unhealthy-key", Key: "token"}
		// IntervalSeconds>0 exercises the interval() configured branch.
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: true, IntervalSeconds: 5}
	})
	get := func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-unhealthy"}, &mp)
		return mp.Status.Conditions
	}
	// A transient probe error does not flip Ready.
	expectReady(t, get, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), agentryv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != agentryv1alpha1.ReasonProviderUnhealthy {
			return errString("Healthy should be False/ProviderUnhealthy")
		}
		return nil
	})
}

// ---- ModelProvider fallback validation ----

func TestModelProvider_FallbackMissingIsNotReady(t *testing.T) {
	mkSecret(t, "mp-fbmiss-key")
	mkProvider(t, "mp-fbmiss", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-fbmiss-key", Key: "token"}
		mp.Spec.Fallback = []agentryv1alpha1.LocalObjectReference{{Name: "no-such-provider"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-fbmiss"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonFallbackIneligible)
}

func TestModelProvider_FallbackTypeMismatchIsNotReady(t *testing.T) {
	mkSecret(t, "mp-fbtype-a-key")
	mkSecret(t, "mp-fbtype-b-key")
	mkProvider(t, "mp-fbtype-b", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.Type = "anthropic"
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-fbtype-b-key", Key: "token"}
	})
	mkProvider(t, "mp-fbtype-a", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.Type = "openai"
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-fbtype-a-key", Key: "token"}
		mp.Spec.Fallback = []agentryv1alpha1.LocalObjectReference{{Name: "mp-fbtype-b"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-fbtype-a"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonFallbackIneligible)
}

// ---- AgentClass FQDN + host validation ----

func TestAgentClass_AllowedHostsUnsupportedByCNI(t *testing.T) {
	ac := &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-hosts"}}
	ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The envtest apiserver exposes no Cilium/Calico groups, so FQDN policy is
	// unsupported; the class stays Ready (allowedHosts is advisory) but the
	// FQDNPolicySupported condition is False.
	expectReady(t, func() []metav1.Condition {
		var got agentryv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hosts"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonAllReferencesResolved)
	eventually(t, func() error {
		var got agentryv1alpha1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hosts"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, agentryv1alpha1.ConditionFQDNPolicySupported)
		if c == nil || c.Status != metav1.ConditionFalse ||
			c.Reason != agentryv1alpha1.ReasonFQDNPolicyUnsupported {
			return errString("FQDNPolicySupported should be False/FQDNPolicyUnsupported")
		}
		return nil
	})
}

func TestAgentClass_InvalidHostIsNotReady(t *testing.T) {
	ac := &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-badhost"}}
	ac.Spec.Network.Egress.AllowedHosts = []string{"not a valid host"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	expectReady(t, func() []metav1.Condition {
		var got agentryv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-badhost"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidReference)
}

// ---- GatewayActivityClient (httpClient + NamespaceActivity) ----

func TestGatewayActivityClient_FansOutAndCaches(t *testing.T) {
	ns := "gw-activity-ns"
	if err := testClient.Create(ctxT(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	pki := newActivatorPKI(t)
	// Server cert SAN must match the ServerName the client pins.
	serving := pki.issue(t, gatewayServiceName+"."+ns+".svc.cluster.local")
	// Client identity files (loaded by httpClient()).
	clientCert := pki.issue(t, "agentry-controller."+ns+".svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, clientCert)

	now := time.Now().UTC()
	var hits int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(ReplicaActivity{
			StartedAt: now,
			Agents:    map[string]AgentActivity{"agent-x": {GatewayTraffic: &now}},
		})
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	// A gateway Pod whose PodIP is the server's listen address becomes the dial
	// target.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-act-0", Namespace: ns, Labels: gatewayPodLabels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	pod.Status.PodIP = host
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("set pod IP: %v", err)
	}

	// Wait for the Pod's IP to land in the informer cache BEFORE the first
	// fan-out: NamespaceActivity caches its result for 15s, so a premature call
	// that saw no target would pin an empty result past this test's window.
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods,
			client.InNamespace(ns), client.MatchingLabels(gatewayPodLabels)); err != nil {
			return err
		}
		for i := range pods.Items {
			if pods.Items[i].Status.PodIP == host {
				return nil
			}
		}
		return errString("gateway pod IP not yet in cache")
	})

	g := &GatewayActivityClient{
		Reader: testClient, OperatorNamespace: ns,
		CertFile: certFile, KeyFile: keyFile, CAFile: caFile, Port: port,
	}
	reachable, total, err := g.NamespaceActivity(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("activity error: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected total=1, got %d", total)
	}
	if len(reachable) != 1 {
		t.Fatal("total=1 but no reachable replica (dial failed)")
	}
	if _, ok := reachable[0].Agents["agent-x"]; !ok {
		t.Fatal("agent data missing")
	}

	// A second call within the cache window must not hit the server again.
	before := atomic.LoadInt32(&hits)
	if _, _, err := g.NamespaceActivity(context.Background(), "team-a"); err != nil {
		t.Fatalf("cached call: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != before {
		t.Errorf("second call should be served from cache, hits went %d -> %d", before, got)
	}
}

func TestGatewayActivityClient_HTTPClientError(t *testing.T) {
	// No gateway Pods in this namespace, but a bad CA file makes httpClient()
	// fail, surfacing an error from NamespaceActivity.
	g := &GatewayActivityClient{
		Reader: testClient, OperatorNamespace: "empty-gw-ns",
		CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key", CAFile: "/nonexistent/ca.crt",
	}
	if _, _, err := g.NamespaceActivity(context.Background(), "team-a"); err == nil {
		t.Error("expected an error from a missing client cert")
	}
}
