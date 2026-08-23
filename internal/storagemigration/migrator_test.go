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

package storagemigration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	webhookconversion "sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
	"sigs.k8s.io/yaml"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const timeout = 20 * time.Second

var (
	testClient client.Client
	testScheme *runtime.Scheme
)

// TestMain starts an envtest whose CRDs store at v1alpha1, the shape of a
// cluster that ran a release before v0.6.0, with the conversion webhook
// served locally so both versions can be written. The test then flips the
// storage version the way "kubectl apply -f crds/" does and runs the
// migrator against objects that are really stored as v1alpha1 bytes.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	testScheme = runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, kaalmv1alpha1.AddToScheme, kaalmv1beta1.AddToScheme, apiextensionsv1.AddToScheme,
	} {
		if err := add(testScheme); err != nil {
			panic(err)
		}
	}
	crds, err := loadCRDs(filepath.Join("..", "..", "config", "crd", "bases"), kaalmv1alpha1.GroupVersion.Version)
	if err != nil {
		panic("load CRDs: " + err.Error())
	}
	testEnv := &envtest.Environment{
		Scheme:            testScheme,
		CRDInstallOptions: envtest.CRDInstallOptions{CRDs: crds},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}
	whOpts := testEnv.WebhookInstallOptions
	srv := webhook.NewServer(webhook.Options{
		Host: whOpts.LocalServingHost, Port: whOpts.LocalServingPort, CertDir: whOpts.LocalServingCertDir,
	})
	srv.Register("/convert", webhookconversion.NewWebhookHandler(testScheme))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.Start(ctx); err != nil {
			panic("webhook server: " + err.Error())
		}
	}()
	started := srv.StartedChecker()
	for deadline := time.Now().Add(timeout); started(nil) != nil; {
		if time.Now().After(deadline) {
			panic("conversion webhook server did not start: " + started(nil).Error())
		}
		time.Sleep(50 * time.Millisecond)
	}
	testClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic("client: " + err.Error())
	}

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// loadCRDs reads the generated CRDs and makes storage the storage version,
// the only change between them and the files the chart ships.
func loadCRDs(dir, storage string) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	files, err := filepath.Glob(filepath.Join(dir, "kaalm.io_*.yaml"))
	if err != nil || len(files) != 6 {
		return nil, fmt.Errorf("want 6 CRD files in %s, got %d (%v)", dir, len(files), err)
	}
	var out []*apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		setStorage(crd, storage)
		out = append(out, crd)
	}
	return out, nil
}

func setStorage(crd *apiextensionsv1.CustomResourceDefinition, storage string) {
	for i := range crd.Spec.Versions {
		crd.Spec.Versions[i].Storage = crd.Spec.Versions[i].Name == storage
	}
}

func TestStorageVersionIsTheHub(t *testing.T) {
	if StorageVersion != kaalmv1beta1.GroupVersion.Version {
		t.Fatalf("StorageVersion %q must be the hub version %q", StorageVersion, kaalmv1beta1.GroupVersion.Version)
	}
	if !(&Migrator{}).NeedLeaderElection() {
		t.Fatal("the migrator must run on the leader only")
	}
}

// countingReader counts List calls so the test can see pagination happen.
type countingReader struct {
	client.Reader
	lists atomic.Int32
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists.Add(1)
	return c.Reader.List(ctx, list, opts...)
}

// TestMigrate is one ordered story against the shared envtest: objects
// stored at v1alpha1, the CRDs flipped to store at v1beta1, one pass that
// moves everything, and a second pass that moves nothing.
func TestMigrate(t *testing.T) {
	ctx := context.Background()
	const classes = 5

	// Objects written while v1alpha1 is the storage version are stored as
	// v1alpha1 bytes, whichever version the client used.
	for i := range classes {
		ac := &kaalmv1beta1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("mig-class-%d", i)},
			Spec:       kaalmv1beta1.AgentClassSpec{Image: kaalmv1beta1.AgentClassImage{DefaultImage: "example/agent:1"}},
		}
		if err := testClient.Create(ctx, ac); err != nil {
			t.Fatalf("create AgentClass: %v", err)
		}
	}
	mp := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-provider", Namespace: "default"},
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://api.example.com",
			CredentialsRef: kaalmv1alpha1.SecretKeyReference{Name: "k", Key: "apiKey"},
		},
	}
	if err := testClient.Create(ctx, mp); err != nil {
		t.Fatalf("create ModelProvider: %v", err)
	}

	// Before the CRDs store at v1beta1 the migrator refuses: trimming
	// storedVersions then would claim a migration that did not happen.
	err := (&Migrator{Reader: testClient, Client: testClient}).Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "apply the chart's CRDs first") {
		t.Fatalf("Migrate before the CRD flip: err = %v, want the storage-version refusal", err)
	}
	for _, k := range kinds {
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{"v1alpha1"}) {
			t.Fatalf("%s storedVersions = %v before the flip, want [v1alpha1]", k.crdName, stored)
		}
	}

	// Step 1 of the upgrade: the CRDs now store at v1beta1. The apiserver
	// appends the new storage version to storedVersions at once, and its
	// serving handler picks the change up a moment later; the sentinel's
	// empty patch bumping resourceVersion is the sign that it has, because
	// a write happens only when the stored bytes differ from the new
	// encoding.
	for _, k := range kinds {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := testClient.Get(ctx, client.ObjectKey{Name: k.crdName}, &crd); err != nil {
			t.Fatal(err)
		}
		setStorage(&crd, StorageVersion)
		if err := testClient.Update(ctx, &crd); err != nil {
			t.Fatalf("flip %s: %v", k.crdName, err)
		}
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{"v1alpha1", "v1beta1"}) {
			t.Fatalf("%s storedVersions = %v after the flip, want [v1alpha1 v1beta1]", k.crdName, stored)
		}
	}
	sentinel := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "mig-class-0"}}
	eventually(t, func() error {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(sentinel), sentinel); err != nil {
			return err
		}
		was := sentinel.ResourceVersion
		if err := testClient.Patch(ctx, sentinel, client.RawPatch(types.MergePatchType, []byte("{}"))); err != nil {
			return err
		}
		if sentinel.ResourceVersion == was {
			return errors.New("sentinel not yet re-encoded")
		}
		return nil
	})

	// One pass: every other object is rewritten (resourceVersion bumps), the
	// sentinel is already at v1beta1 and is left alone, the lists were
	// paginated, the CRDs are trimmed, and the metric counts the rewrites.
	before := resourceVersions(t)
	moved := testutil.ToFloat64(migratedObjects.WithLabelValues("AgentClass"))
	reader := &countingReader{Reader: testClient}
	m := &Migrator{Reader: reader, Client: testClient, PageSize: 2}
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	after := resourceVersions(t)
	for name, rv := range before {
		if name == "AgentClass/mig-class-0" {
			if after[name] != rv {
				t.Errorf("%s was already at v1beta1 and must not be rewritten (rv %s -> %s)", name, rv, after[name])
			}
			continue
		}
		if after[name] == rv {
			t.Errorf("%s not rewritten (rv still %s)", name, rv)
		}
	}
	if got := reader.lists.Load(); got < 3 {
		t.Errorf("AgentClass list with PageSize 2 over %d objects made %d List calls, want pagination", classes, got)
	}
	for _, k := range kinds {
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{StorageVersion}) {
			t.Errorf("%s storedVersions = %v after migration, want [%s]", k.crdName, stored, StorageVersion)
		}
	}
	if got := testutil.ToFloat64(migratedObjects.WithLabelValues("AgentClass")) - moved; got != classes-1 {
		t.Errorf("kaalm_storage_migrated_objects_total{kind=AgentClass} rose by %v, want %d", got, classes-1)
	}
	if got := testutil.ToFloat64(migratedObjects.WithLabelValues("ModelProvider")); got != 1 {
		t.Errorf("kaalm_storage_migrated_objects_total{kind=ModelProvider} = %v, want 1", got)
	}

	// A second pass is a no-op: every CRD is skipped, nothing is written.
	reader.lists.Store(0)
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := reader.lists.Load(); got != 0 {
		t.Errorf("second pass listed %d times, want 0 (every CRD already at [%s])", got, StorageVersion)
	}
	if again := resourceVersions(t); !mapsEqual(after, again) {
		t.Errorf("second pass rewrote objects: %v -> %v", after, again)
	}
}

// TestStartRetriesUntilContextEnds proves the failure policy: a pass that
// cannot complete is retried with backoff and never returned as an error,
// so the manager keeps running.
func TestStartRetriesUntilContextEnds(t *testing.T) {
	var gets atomic.Int32
	failing := fake.NewClientBuilder().WithScheme(testScheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			gets.Add(1)
			return errors.New("apiserver unavailable")
		},
	}).Build()
	m := &Migrator{Reader: failing, Client: failing, MaxBackoff: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start returned %v, want nil on context end", err)
	}
	if got := gets.Load(); got < 3 {
		t.Fatalf("Start retried %d times in 200ms with a 10ms cap, want several", got)
	}
}

func storedVersions(t *testing.T, crdName string) []string {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: crdName}, &crd); err != nil {
		t.Fatal(err)
	}
	return crd.Status.StoredVersions
}

// resourceVersions maps "Kind/name" to resourceVersion for every test object.
func resourceVersions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var classes kaalmv1beta1.AgentClassList
	if err := testClient.List(context.Background(), &classes); err != nil {
		t.Fatal(err)
	}
	for _, c := range classes.Items {
		out["AgentClass/"+c.Name] = c.ResourceVersion
	}
	var providers kaalmv1beta1.ModelProviderList
	if err := testClient.List(context.Background(), &providers); err != nil {
		t.Fatal(err)
	}
	for _, p := range providers.Items {
		out["ModelProvider/"+p.Name] = p.ResourceVersion
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

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
