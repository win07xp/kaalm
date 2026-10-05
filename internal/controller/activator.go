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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/tlsutil"
)

// ActivatorServer serves the controller's :9443 endpoints: kubelet probes
// (cert-less) and POST /v1/activate/{namespace}/{agentName} (gateway SAN
// required). It runs on EVERY controller replica, not only the leader: the
// handler is deliberately thin, patching kaalm.io/wake=true (with
// kaalm.io/wake-trigger=channel) on the target Agent so the leader's existing
// watch drives the actual wake. See
// docs/src/gateways/user/activation-and-activity.md (The Activator).
//
// It never depends on the manager's cache: the handler writes without
// reading, and Listen returns a manager.Server, which the manager starts
// with the health probes, before the caches. Controller readiness includes
// the activator, and the same readiness gates the conversion webhook that
// the caches may need in order to sync (an upgrade from before the v1beta1
// graduation still stores v1alpha1 objects). An activator that waited on
// the caches would deadlock that upgrade.
type ActivatorServer struct {
	Client            client.Client
	OperatorNamespace string
	Addr              string
	CertFile          string
	KeyFile           string
	CAFile            string

	// listening is true from the moment Listen binds the listener until the
	// server closes it; ReadyCheck reads it.
	listening atomic.Bool
}

// activatorShutdownTimeout bounds the graceful shutdown of in-flight wakes.
const activatorShutdownTimeout = 5 * time.Second

// Listen loads the TLS material, binds the listener, and returns the
// activator as a manager.Server to pass to mgr.Add. Binding here, before the
// manager starts, makes a bad address or certificate fail at startup, and
// the readiness check passes from this point: the kernel queues any
// connection that arrives before the server starts serving.
func (s *ActivatorServer) Listen() (*manager.Server, error) {
	// The serving cert is re-read per handshake and ClientCAs rebuilt per
	// connection, so cert-manager rotation applies without a restart.
	// Probes present no cert; the activate handler enforces per-path.
	loader := &tlsutil.CertLoader{CertFile: s.CertFile, KeyFile: s.KeyFile, CAFile: s.CAFile}
	tlsCfg, err := loader.ServerMTLSConfig(tls.VerifyClientCertIfGiven)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	mux.HandleFunc("/v1/activate/", s.handleActivate)

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return nil, err
	}
	s.listening.Store(true)
	tracked := &closeTrackingListener{Listener: ln, onClose: func() { s.listening.Store(false) }}

	timeout := activatorShutdownTimeout
	return &manager.Server{
		Name:            "activator",
		Server:          &http.Server{Handler: mux, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second},
		Listener:        tls.NewListener(tracked, tlsCfg),
		ShutdownTimeout: &timeout,
	}, nil
}

// closeTrackingListener runs onClose once when the listener is closed, which
// http.Server does on shutdown and when Serve fails.
type closeTrackingListener struct {
	net.Listener
	once    sync.Once
	onClose func()
}

func (l *closeTrackingListener) Close() error {
	l.once.Do(l.onClose)
	return l.Listener.Close()
}

// ReadyCheck is the manager's readyz check for the activator: it passes only
// while the :9443 listener is bound, so a replica joins the controller
// Service only once it can take a wake. It reads a flag, so it costs nothing
// per probe. A server that fails after binding ends its Start with an error,
// which stops the manager and exits the process.
func (s *ActivatorServer) ReadyCheck(_ *http.Request) error {
	if !s.listening.Load() {
		return errors.New("activator listener not serving")
	}
	return nil
}

// handleActivate authorizes the gateway SAN and patches the wake annotation.
// It does no lifecycle work itself; the apiserver is the message bus to the
// leader's reconciler.
func (s *ActivatorServer) handleActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	if !s.isGatewayCert(r.TLS.PeerCertificates[0]) {
		http.Error(w, "this path requires the gateway identity", http.StatusForbidden)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v1/activate/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "path must be /v1/activate/{namespace}/{agentName}", http.StatusBadRequest)
		return
	}
	namespace, name := parts[0], parts[1]

	// No read first: the patch alone answers NotFound for a missing Agent,
	// and the client's reads come from a cache that may not have synced.
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	// One patch carries the wake and its trigger, so the reconciler never
	// sees one without the other and can count this wake apart from a manual
	// one (kaalm_wakes_total{trigger}).
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q,%q:%q}}}`,
		kaalmv1beta1.AnnotationWake, kaalmv1beta1.AnnotationTrue,
		kaalmv1beta1.AnnotationWakeTrigger, kaalmv1beta1.AnnotationWakeTriggerChannel)
	if err := s.Client.Patch(r.Context(), agent, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 202 confirms only that the annotation was written; the wake itself is
	// watch-driven on the leader.
	w.WriteHeader(http.StatusAccepted)
}

func (s *ActivatorServer) isGatewayCert(cert *x509.Certificate) bool {
	long := fmt.Sprintf("%s.%s.svc.cluster.local", gatewayServiceName, s.OperatorNamespace)
	short := fmt.Sprintf("%s.%s.svc", gatewayServiceName, s.OperatorNamespace)
	for _, san := range cert.DNSNames {
		if san == long || san == short {
			return true
		}
	}
	return false
}
