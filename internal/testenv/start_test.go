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

package testenv

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func requireAssets(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; make test provides the envtest binaries")
	}
}

// TestStart_AdminUsesBackdatedCredentials proves the apiserver accepts the
// credentials Start provisions.
func TestStart_AdminUsesBackdatedCredentials(t *testing.T) {
	requireAssets(t)
	env := &envtest.Environment{}
	cfg, err := Start(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	if _, ok := env.ControlPlane.APIServer.Authn.(*backdatedCertAuthn); !ok {
		t.Fatalf("authn = %T, want *backdatedCertAuthn", env.ControlPlane.APIServer.Authn)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "default", metav1.GetOptions{}); err != nil {
		t.Fatalf("admin request: %v", err)
	}
}

// TestStart_StopsControlPlaneOnFailure proves a start that fails after the
// control plane is up stops the apiserver and etcd instead of leaving them
// running once the test binary exits.
func TestStart_StopsControlPlaneOnFailure(t *testing.T) {
	requireAssets(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(t.TempDir(), "missing")},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := Start(env); err == nil {
		_ = env.Stop()
		t.Fatal("Start succeeded with a missing CRD path")
	}

	for name, u := range map[string]string{
		"kube-apiserver": env.ControlPlane.APIServer.SecureServing.URL("https", "").Host,
		"etcd":           env.ControlPlane.Etcd.URL.Host,
	} {
		conn, err := net.DialTimeout("tcp", u, time.Second)
		if err == nil {
			_ = conn.Close()
			t.Errorf("%s still listens on %s after the failed start", name, u)
		}
	}
}
