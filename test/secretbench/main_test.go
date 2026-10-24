//go:build secretbench

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

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/testenv"
)

// restCfg and adminClient reach the envtest control plane; nil when
// KUBEBUILDER_ASSETS is unset and the envtest tests skip.
var (
	restCfg     *rest.Config
	adminClient kubernetes.Interface
	nsCounter   atomic.Int64
)

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite starts envtest when its assets are present and stops it on every
// path, so the kube-apiserver and etcd never outlive the test binary.
func runSuite(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	env := &envtest.Environment{}
	apiServer := env.ControlPlane.GetAPIServer()
	apiServer.Out, apiServer.Err = io.Discard, io.Discard
	cfg, err := testenv.Start(env)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "secretbench envtest unavailable:", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	restCfg = cfg
	adminCfg := rest.CopyConfig(cfg)
	adminCfg.QPS, adminCfg.Burst = -1, 0
	adminClient, err = kubernetes.NewForConfig(adminCfg)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "secretbench admin client:", err)
		return 1
	}
	return m.Run()
}

// requireEnvtest skips a test that needs the control plane when its assets
// are absent.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if restCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make secretwatch-bench-unit")
	}
}

// testNamespace creates a fresh namespace for one test.
func testNamespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("bench-test-%d", nsCounter.Add(1))
	_, err := adminClient.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

// createSecret creates a Secret, labeled as a channel credential when
// labeled is true.
func createSecret(t *testing.T, namespace, name string, labeled bool) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string][]byte{"token": newToken()},
	}
	if labeled {
		sec.Labels = map[string]string{kaalmv1beta1.LabelChannelCredential: "true"}
	}
	out, err := adminClient.CoreV1().Secrets(namespace).Create(context.Background(), sec, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	return out
}

// countedClient builds a client through a counting transport.
func countedClient(t *testing.T) (kubernetes.Interface, *countingTransport) {
	t.Helper()
	cfg, counter := countedConfig(restCfg, 100, 200)
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client, counter
}

// eventually polls cond every 20 ms for up to timeout.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stopAndDrain stops a reader and waits until its watches have closed.
func stopAndDrain(t *testing.T, r secretReader, counter *countingTransport) {
	t.Helper()
	r.Stop()
	eventually(t, 10*time.Second, "watches to close after Stop", func() bool {
		return counter.openWatches.Load() == 0
	})
	counter.CloseIdleConnections()
}

// settle waits until the reader has opened watches watches and no GET or
// LIST is in flight or arrived for 500 ms.
func settle(t *testing.T, counter *countingTransport, watches int64) {
	t.Helper()
	last := counter.snapshot()
	quietSince := time.Now()
	eventually(t, 10*time.Second, "requests to settle", func() bool {
		now := counter.snapshot()
		if now != last || counter.inflightNonWatch.Load() != 0 || counter.openWatches.Load() != watches {
			last = now
			quietSince = time.Now()
			return false
		}
		return time.Since(quietSince) >= 500*time.Millisecond
	})
}

// secretKeysIn lists names in one namespace.
func secretKeysIn(namespace string, names ...string) []types.NamespacedName {
	out := make([]types.NamespacedName, len(names))
	for i, n := range names {
		out[i] = types.NamespacedName{Namespace: namespace, Name: n}
	}
	return out
}
