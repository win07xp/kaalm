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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

const testOperatorNamespace = "default"

var (
	testClient client.Client
	testEnv    *envtest.Environment
	fakeHealth *fakeHealthChecker
)

// fakeHealthChecker returns a canned probe result per provider name, defaulting to
// Healthy, so ModelProvider tests never reach a real provider.
type fakeHealthChecker struct {
	mu      sync.Mutex
	results map[string]ProviderProbeResult
}

func newFakeHealth() *fakeHealthChecker {
	return &fakeHealthChecker{results: map[string]ProviderProbeResult{}}
}

func (f *fakeHealthChecker) set(name string, res ProviderProbeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = res
}

func (f *fakeHealthChecker) Probe(
	_ context.Context, provider *agentryv1alpha1.ModelProvider, _ string,
) ProviderProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if res, ok := f.results[provider.Name]; ok {
		return res
	}
	return ProviderProbeResult{Healthy: true}
}

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := agentryv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic("manager: " + err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := SetupIndexers(ctx, mgr); err != nil {
		panic("indexers: " + err.Error())
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		panic(err)
	}
	fakeHealth = newFakeHealth()
	if err := (&AgentClassReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"), Discovery: dc,
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if err := (&ModelProviderReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testOperatorNamespace, Health: fakeHealth,
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic("manager start: " + err.Error())
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("cache sync failed")
	}
	testClient = mgr.GetClient()

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// eventually polls fn until it returns nil or the package timeout elapses.
func eventually(t *testing.T, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %v", timeout, last)
}

func condition(conds []metav1.Condition, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(conds, condType)
}
