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
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGatewayActivityClient_NonOKResponse covers the branch where a replica is
// reachable but answers with a non-200 status: it is treated as unreachable.
func TestGatewayActivityClient_NonOKResponse(t *testing.T) {
	ns := "gw-500-ns"
	if err := testClient.Create(ctxT(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	pki := newActivatorPKI(t)
	serving := pki.issue(t, gatewayServiceName+"."+ns+".svc.cluster.local")
	clientCert := pki.issue(t, "agentry-controller."+ns+".svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, clientCert)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-500-0", Namespace: ns, Labels: gatewayPodLabels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	pod.Status.PodIP = host
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("set pod IP: %v", err)
	}

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
	if len(reachable) != 0 {
		t.Fatal("a 500 response must be treated as unreachable")
	}
}

// TestGatewayActivityClient_DefaultPortUnreachable covers the default port and
// unreachable-replica branches.
func TestGatewayActivityClient_DefaultPortUnreachable(t *testing.T) {
	ns := "gw-unreach-ns"
	if err := testClient.Create(ctxT(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	pki := newActivatorPKI(t)
	clientCert := pki.issue(t, "agentry-controller."+ns+".svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, clientCert)

	// A gateway Pod with an IP but nothing listening on the default port.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-dead-0", Namespace: ns, Labels: gatewayPodLabels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	pod.Status.PodIP = "127.0.0.1"
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("set pod IP: %v", err)
	}

	// Wait for the Pod IP to reach the informer cache before the first fan-out
	// (the client caches its result for 15s).
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods,
			client.InNamespace(ns), client.MatchingLabels(gatewayPodLabels)); err != nil {
			return err
		}
		for i := range pods.Items {
			if pods.Items[i].Status.PodIP == "127.0.0.1" {
				return nil
			}
		}
		return errString("gateway pod IP not yet in cache")
	})

	// Port 0 exercises the default-gatewayPort branch; the dial then fails.
	g := &GatewayActivityClient{
		Reader: testClient, OperatorNamespace: ns,
		CertFile: certFile, KeyFile: keyFile, CAFile: caFile, Port: 0,
	}
	reachable, total, err := g.NamespaceActivity(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("activity error: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected the pod to be enumerated as a target, got total=%d", total)
	}
	if len(reachable) != 0 {
		t.Fatal("an unreachable replica must not appear in reachable")
	}
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

// ---- activity merge ----

func TestMergedActivity_Sources(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	t1 := time.Now().Add(-time.Hour)
	newer := time.Now().Add(-time.Minute)
	reachable := []ReplicaActivity{
		{Agents: map[string]AgentActivity{"a": {GatewayTraffic: &t0, Heartbeat: &t1}}},
		{Agents: map[string]AgentActivity{"a": {GatewayTraffic: &newer}}},
	}
	// gatewayTraffic (default) picks the most recent traffic across replicas.
	if got := mergedActivity(reachable, "a", "gatewayTraffic"); got == nil || !got.Equal(newer) {
		t.Errorf("gatewayTraffic merge wrong: %v", got)
	}
	// agentHeartbeat ignores traffic.
	if got := mergedActivity(reachable, "a", "agentHeartbeat"); got == nil || !got.Equal(t1) {
		t.Errorf("agentHeartbeat merge wrong: %v", got)
	}
	// both takes the newer of traffic and heartbeat.
	if got := mergedActivity(reachable, "a", "both"); got == nil || !got.Equal(newer) {
		t.Errorf("both merge wrong: %v", got)
	}
	// unknown agent -> nil.
	if got := mergedActivity(reachable, "missing", "both"); got != nil {
		t.Errorf("missing agent must merge to nil, got %v", got)
	}
}

// TestGatewayHTTPClient covers the client builder: a cached second call plus the
// CA read and parse failures.
func TestGatewayHTTPClient(t *testing.T) {
	pki := newActivatorPKI(t)
	cert := pki.issue(t, "agentry-controller.default.svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, cert)

	g := &GatewayActivityClient{OperatorNamespace: "default", CertFile: certFile, KeyFile: keyFile, CAFile: caFile}
	c1, err := g.httpClient()
	if err != nil || c1 == nil {
		t.Fatalf("first httpClient: %v", err)
	}
	c2, err := g.httpClient()
	if err != nil {
		t.Fatalf("second httpClient: %v", err)
	}
	if c1 != c2 {
		t.Error("httpClient must memoize the constructed client")
	}

	dir := t.TempDir()
	badPEM := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPEM, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&GatewayActivityClient{CertFile: certFile, KeyFile: keyFile, CAFile: filepath.Join(dir, "absent")}).httpClient(); err == nil {
		t.Error("a missing CA file must fail httpClient")
	}
	if _, err := (&GatewayActivityClient{CertFile: certFile, KeyFile: keyFile, CAFile: badPEM}).httpClient(); err == nil {
		t.Error("a non-PEM CA must fail httpClient")
	}
}
