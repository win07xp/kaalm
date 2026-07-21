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
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleActivate_Guards drives handleActivate directly, exercising the
// method, client-cert, SAN, and path-shape guards without a live listener.
func TestHandleActivate_Guards(t *testing.T) {
	s := &ActivatorServer{OperatorNamespace: "agentry-system"}

	gatewayTLS := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"agentry-gateway.agentry-system.svc.cluster.local"}},
	}}
	agentTLS := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"sup.team-a.svc.cluster.local"}},
	}}

	cases := []struct {
		name   string
		method string
		path   string
		tls    *tls.ConnectionState
		want   int
	}{
		{"GET not allowed", http.MethodGet, "/v1/activate/default/a", gatewayTLS, http.StatusMethodNotAllowed},
		{"no client cert", http.MethodPost, "/v1/activate/default/a", nil, http.StatusUnauthorized},
		{"wrong identity", http.MethodPost, "/v1/activate/default/a", agentTLS, http.StatusForbidden},
		{"bad path shape", http.MethodPost, "/v1/activate/only-one-segment", gatewayTLS, http.StatusBadRequest},
		{"empty segment", http.MethodPost, "/v1/activate/default/", gatewayTLS, http.StatusBadRequest},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.TLS = c.tls
		w := httptest.NewRecorder()
		s.handleActivate(w, req)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// TestHandleActivate_AgentNotFound exercises the Get/NotFound -> 404 path with
// the shared envtest client.
func TestHandleActivate_AgentNotFound(t *testing.T) {
	s := &ActivatorServer{Client: testClient, OperatorNamespace: testSystemNamespace}
	req := httptest.NewRequest(http.MethodPost, "/v1/activate/default/no-such-agent-xyz", strings.NewReader(""))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"agentry-gateway." + testSystemNamespace + ".svc.cluster.local"}},
	}}
	w := httptest.NewRecorder()
	s.handleActivate(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing agent: status = %d, want 404", w.Code)
	}
}

func TestIsGatewayCert_ShortSAN(t *testing.T) {
	s := &ActivatorServer{OperatorNamespace: "agentry-system"}
	// The short svc SAN form is also accepted.
	if !s.isGatewayCert(&x509.Certificate{DNSNames: []string{"agentry-gateway.agentry-system.svc"}}) {
		t.Error("short svc SAN must be recognized as the gateway identity")
	}
	if s.isGatewayCert(&x509.Certificate{DNSNames: []string{"unrelated.example.com"}}) {
		t.Error("an unrelated SAN must not be recognized as the gateway")
	}
}
