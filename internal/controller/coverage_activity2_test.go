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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

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
