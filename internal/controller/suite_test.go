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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	webhookconversion "sigs.k8s.io/controller-runtime/pkg/webhook/conversion"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/mcp"
	"github.com/win07xp/kaalm/internal/secretwatch"
	"github.com/win07xp/kaalm/internal/testenv"
)

const (
	testOperatorNamespace = "default"
	// testSystemNamespace is the AgentReconciler's forbidden operator
	// namespace; kept distinct from testOperatorNamespace so ModelProvider
	// credential Secrets (in default) and Agent workloads (also in default)
	// do not collide with the system-namespace guard.
	testSystemNamespace = "kaalm-system"
)

var (
	testClient client.Client
	// testAPIReader reads straight from the apiserver. The test read helpers
	// use it: testClient reads from the manager's cache, which can miss an
	// object the test created a moment ago.
	testAPIReader  client.Reader
	testEnv        *envtest.Environment
	fakeHealth     *fakeHealthChecker
	fakeToolHealth *fakeToolHealthChecker
	fakeActivity   *fakeActivityClient
)

// fakeActivityClient serves canned gateway activity data. total 0 with no
// error models "no gateway pods"; empty reachable with total > 0 models "all
// replicas unreachable".
type fakeActivityClient struct {
	mu        sync.Mutex
	reachable []ReplicaActivity
	total     int
}

func (f *fakeActivityClient) set(reachable []ReplicaActivity, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable = reachable
	f.total = total
}

func (f *fakeActivityClient) NamespaceActivity(context.Context, string) ([]ReplicaActivity, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reachable, f.total, nil
}

// fakeHealthChecker returns a canned probe result per provider name, defaulting to
// Healthy, so ModelProvider tests never reach a real provider.
type fakeHealthChecker struct {
	mu      sync.Mutex
	results map[string]ProviderProbeResult
	calls   map[string]int
}

func newFakeHealth() *fakeHealthChecker {
	return &fakeHealthChecker{
		results: map[string]ProviderProbeResult{},
		calls:   map[string]int{},
	}
}

func (f *fakeHealthChecker) set(name string, res ProviderProbeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = res
}

// count reports how many times Probe was invoked for a provider, so tests can
// assert the probe was (or was not) reached.
func (f *fakeHealthChecker) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeHealthChecker) Probe(
	_ context.Context, provider *kaalmv1beta1.ModelProvider, _ string,
) ProviderProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[provider.Name]++
	if res, ok := f.results[provider.Name]; ok {
		return res
	}
	return ProviderProbeResult{Healthy: true}
}

// fakeToolHealthChecker is fakeHealthChecker's ToolProvider twin: a canned
// probe result per provider name, defaulting to Healthy, recording the
// credential each probe carried.
type fakeToolHealthChecker struct {
	mu          sync.Mutex
	results     map[string]ToolProbeResult
	calls       map[string]int
	credentials map[string]string
}

func newFakeToolHealth() *fakeToolHealthChecker {
	return &fakeToolHealthChecker{
		results:     map[string]ToolProbeResult{},
		calls:       map[string]int{},
		credentials: map[string]string{},
	}
}

func (f *fakeToolHealthChecker) set(name string, res ToolProbeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[name] = res
}

func (f *fakeToolHealthChecker) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// credential reports the credential value the most recent probe for name
// carried, so tests can prove what was (or was not) resolved.
func (f *fakeToolHealthChecker) credential(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentials[name]
}

func (f *fakeToolHealthChecker) Probe(
	_ context.Context, provider *kaalmv1beta1.ToolProvider, credential string,
) ToolProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[provider.Name]++
	f.credentials[provider.Name] = credential
	if res, ok := f.results[provider.Name]; ok {
		return res
	}
	return ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true},
		MCPRevision: mcp.ModernRevision}
}

// fqdnSupportedInTests stands in for the CNI probe on the Agent and
// AgentTask reconcilers: the suite loads a CiliumNetworkPolicy CRD, so the
// FQDN policy is synthesized.
func fqdnSupportedInTests() (bool, error) { return true, nil }

// withoutGroup hides one API group from a discovery client.
type withoutGroup struct {
	discovery.DiscoveryInterface
	group string
}

func (w withoutGroup) ServerGroups() (*metav1.APIGroupList, error) {
	list, err := w.DiscoveryInterface.ServerGroups()
	if list == nil {
		return nil, err
	}
	out := &metav1.APIGroupList{}
	for _, g := range list.Groups {
		if g.Name != w.group {
			out.Groups = append(out.Groups, g)
		}
	}
	return out, err
}

// ServerResourcesForGroupVersion answers NotFound for a version of the hidden
// group, as the API server does for a group version it does not serve.
func (w withoutGroup) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	parsed, err := schema.ParseGroupVersion(gv)
	if err == nil && parsed.Group == w.group {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: w.group}, "")
	}
	return w.DiscoveryInterface.ServerResourcesForGroupVersion(gv)
}

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// setupFailed reports a suite setup failure and returns the exit code, so
// runSuite's deferred cleanup still stops envtest.
func setupFailed(step string, err error) int {
	fmt.Fprintf(os.Stderr, "suite setup: %s: %v\n", step, err)
	return 1
}

// runSuite starts envtest and the manager, runs the tests, and stops envtest
// on every path: a setup failure returns instead of panicking, so the
// kube-apiserver and etcd never outlive the test binary.
func runSuite(m *testing.M) int {
	// The scheme holds both API versions so envtest sees every kind as
	// convertible and installs the conversion webhook into the CRDs, pointed
	// at the local webhook server below. The reconcilers under test work at
	// v1beta1, the hub and storage version, exactly as the controller does in
	// a cluster; conversion_test.go is the one place that writes at v1alpha1
	// and proves the webhook through the apiserver.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := kaalmv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := cmapi.AddToScheme(scheme); err != nil {
		panic(err)
	}

	testEnv = &envtest.Environment{
		Scheme: scheme,
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testenv.Start(testEnv)
	if err != nil {
		return setupFailed("start envtest", err)
	}
	defer func() { _ = testEnv.Stop() }()

	whOpts := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    whOpts.LocalServingHost,
			Port:    whOpts.LocalServingPort,
			CertDir: whOpts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return setupFailed("manager", err)
	}
	mgr.GetWebhookServer().Register("/convert", webhookconversion.NewWebhookHandler(scheme))

	// Recoverable gates poll fast in tests (production default is 30s).
	gateRequeue = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := SetupIndexers(ctx, mgr); err != nil {
		return setupFailed("indexers", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return setupFailed("discovery client", err)
	}
	// User-namespace Secret reads go through the production path: one
	// name-filtered watch per referenced Secret.
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return setupFailed("clientset", err)
	}
	secretWatcher := secretwatch.New(ctx, clientset)
	secretSource := secretwatch.NewReader(secretWatcher)
	fakeHealth = newFakeHealth()
	if err := (&AgentClassReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		// The class sees the real envtest discovery minus cilium.io, so its
		// FQDNPolicySupported=False path stays covered while the Agent and
		// AgentTask reconcilers below synthesize policies.
		FQDNSupport: NewFQDNProbe(withoutGroup{dc, "cilium.io"}).Supported,
		// No test creates this Secret except cert_cleanup_test.go, so other
		// classes report CertificateCleanup=Unknown.
		CertCleanup: &CertCleanupCheck{
			Reader: mgr.GetClient(),
			Secret: types.NamespacedName{Namespace: testOperatorNamespace, Name: ControllerTLSSecretName},
		},
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("AgentClassReconciler", err)
	}
	if err := (&ModelProviderReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testOperatorNamespace, Health: fakeHealth,
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("ModelProviderReconciler", err)
	}
	fakeToolHealth = newFakeToolHealth()
	if err := (&ToolProviderReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testOperatorNamespace, Health: fakeToolHealth,
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("ToolProviderReconciler", err)
	}
	fakeActivity = &fakeActivityClient{}
	if err := (&AgentReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testSystemNamespace,
		SecretReader:      secretSource,
		Activity:          fakeActivity,
		FQDNSupport:       fqdnSupportedInTests,
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("AgentReconciler", err)
	}
	if err := (&AgentTaskReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testSystemNamespace,
		SecretReader:      secretSource,
		FQDNSupport:       fqdnSupportedInTests,
		deadlineFor:       shortProvisioningDeadline,
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("AgentTaskReconciler", err)
	}
	disconnectTimeout = 3 * time.Second
	if err := (&AgentChannelReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("test"),
		OperatorNamespace: testSystemNamespace,
		SecretReader:      secretSource,
		SecretChanges:     secretWatcher,
	}).SetupWithManager(mgr); err != nil {
		return setupFailed("AgentChannelReconciler", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			// Cancelling unblocks the cache sync below, or fails the tests
			// already running, and runSuite then stops envtest.
			fmt.Fprintf(os.Stderr, "manager start: %v\n", err)
			cancel()
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		return setupFailed("cache sync", errors.New("caches did not sync"))
	}
	// conversion_test.go writes at v1alpha1, which converts through the
	// webhook, so it must be listening before any test runs.
	started := mgr.GetWebhookServer().StartedChecker()
	for deadline := time.Now().Add(timeout); started(nil) != nil; {
		if time.Now().After(deadline) {
			return setupFailed("conversion webhook server", started(nil))
		}
		time.Sleep(50 * time.Millisecond)
	}
	testClient = mgr.GetClient()
	testAPIReader = mgr.GetAPIReader()

	// The system namespace must exist for the SystemNamespaceForbidden test.
	sysNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testSystemNamespace}}
	if err := testClient.Create(ctx, sysNS); err != nil {
		return setupFailed("create system namespace", err)
	}

	return m.Run()
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

// consistently polls fn for the whole of d and fails the first time it
// returns an error. It pins a state that must hold across several
// reconcile passes, not just at one read.
func consistently(t *testing.T, d time.Duration, fn func() error) {
	t.Helper()
	start := time.Now()
	for {
		if err := fn(); err != nil {
			t.Fatalf("condition broke after %s: %v", time.Since(start).Round(time.Millisecond), err)
		}
		if time.Since(start) >= d {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func condition(conds []metav1.Condition, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(conds, condType)
}

// shortDeadlineLabel marks a test task whose provisioning deadline is one
// second, so a deadline test need not change state the manager reads.
const shortDeadlineLabel = "test.kaalm.io/short-provisioning-deadline"

// shortProvisioningDeadline is the suite's AgentTaskReconciler.deadlineFor:
// one second for a task carrying shortDeadlineLabel, the default otherwise.
func shortProvisioningDeadline(task *kaalmv1beta1.AgentTask) time.Duration {
	if task.Labels[shortDeadlineLabel] == "true" {
		return time.Second
	}
	return 0
}

// withShortDeadline labels task for shortProvisioningDeadline.
func withShortDeadline(task *kaalmv1beta1.AgentTask) {
	if task.Labels == nil {
		task.Labels = map[string]string{}
	}
	task.Labels[shortDeadlineLabel] = "true"
}
