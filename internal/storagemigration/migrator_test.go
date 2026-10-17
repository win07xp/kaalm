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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/testenv"
)

const timeout = 20 * time.Second

var (
	testClient client.Client
	testScheme *runtime.Scheme
)

// TestMain starts an envtest with two CRDs of the test's own, each served at
// v1 and v2 with v1 as the storage version: the shape of a cluster before a
// release that moves the storage version. Both versions share one schema, so
// the apiserver converts between them without a webhook. The test then
// flips the storage version the way "kubectl apply -f crds/" does and runs
// the migrator against objects that are really stored as v1 bytes.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// setupFailed reports a suite setup failure and returns the exit code, so
// runSuite's deferred cleanup still stops envtest.
func setupFailed(step string, err error) int {
	fmt.Fprintf(os.Stderr, "suite setup: %s: %v\n", step, err)
	return 1
}

// runSuite is TestMain's body. It stops envtest on every path: a setup
// failure returns instead of panicking, so the kube-apiserver and etcd never
// outlive the test binary.
func runSuite(m *testing.M) int {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	testScheme = runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, kaalmv1beta1.AddToScheme, apiextensionsv1.AddToScheme,
	} {
		if err := add(testScheme); err != nil {
			panic(err)
		}
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, k := range testKinds {
		crds = append(crds, testCRD(k, oldVersion))
	}
	testEnv := &envtest.Environment{
		Scheme:            testScheme,
		CRDInstallOptions: envtest.CRDInstallOptions{CRDs: crds},
	}
	cfg, err := testenv.Start(testEnv)
	if err != nil {
		return setupFailed("start envtest", err)
	}
	defer func() { _ = testEnv.Stop() }()
	testClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		return setupFailed("client", err)
	}

	return m.Run()
}

// The test's own group, versions, and kinds. The migrator handles every
// kind as unstructured, so nothing in it is specific to Kaalm's CRDs.
const (
	testGroup  = "migration.test.kaalm.io"
	oldVersion = "v1"
	newVersion = "v2"
)

var testKinds = []entry{
	{crdName: "widgets." + testGroup, kind: "Widget"},
	{crdName: "gadgets." + testGroup, kind: "Gadget"},
}

// testCRD builds a cluster-scoped CRD served at both versions with one
// schema that keeps every field, storing at storage.
func testCRD(k entry, storage string) *apiextensionsv1.CustomResourceDefinition {
	plural := strings.TrimSuffix(k.crdName, "."+testGroup)
	keep := true
	validation := &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
		Type: "object", XPreserveUnknownFields: &keep,
	}}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: k.crdName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: testGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: plural, Kind: k.kind, ListKind: k.kind + "List",
			},
			Scope: apiextensionsv1.ClusterScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: oldVersion, Served: true, Schema: validation},
				{Name: newVersion, Served: true, Schema: validation},
			},
		},
	}
	setStorage(crd, storage)
	return crd
}

func setStorage(crd *apiextensionsv1.CustomResourceDefinition, storage string) {
	for i := range crd.Spec.Versions {
		crd.Spec.Versions[i].Storage = crd.Spec.Versions[i].Name == storage
	}
}

// testMigrator migrates the test's CRDs to newVersion.
func testMigrator(r client.Reader, c client.Client) *Migrator {
	return &Migrator{Reader: r, Client: c, kinds: testKinds, version: newVersion}
}

// newObject returns an unstructured object of kind at version.
func newObject(kind, version, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: testGroup, Version: version, Kind: kind})
	u.SetName(name)
	return u
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
// stored at v1, the CRDs flipped to store at v2, one pass that moves
// everything, and a second pass that moves nothing.
func TestMigrate(t *testing.T) {
	ctx := context.Background()
	const widgets = 5

	// Objects written while v1 is the storage version are stored as v1
	// bytes, whichever version the client used.
	for i := range widgets {
		w := newObject("Widget", newVersion, fmt.Sprintf("mig-widget-%d", i))
		if err := testClient.Create(ctx, w); err != nil {
			t.Fatalf("create Widget: %v", err)
		}
	}
	if err := testClient.Create(ctx, newObject("Gadget", oldVersion, "mig-gadget")); err != nil {
		t.Fatalf("create Gadget: %v", err)
	}

	// Before the CRDs store at v2 the migrator refuses: trimming
	// storedVersions then would claim a migration that did not happen.
	err := testMigrator(testClient, testClient).Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "apply the chart's CRDs first") {
		t.Fatalf("Migrate before the CRD flip: err = %v, want the storage-version refusal", err)
	}
	for _, k := range testKinds {
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{oldVersion}) {
			t.Fatalf("%s storedVersions = %v before the flip, want [%s]", k.crdName, stored, oldVersion)
		}
	}

	// Step 1 of the upgrade: the CRDs now store at v2. The apiserver
	// appends the new storage version to storedVersions at once, and its
	// serving handler picks the change up a moment later; the sentinel's
	// empty patch bumping resourceVersion is the sign that it has, because
	// a write happens only when the stored bytes differ from the new
	// encoding.
	for _, k := range testKinds {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := testClient.Get(ctx, client.ObjectKey{Name: k.crdName}, &crd); err != nil {
			t.Fatal(err)
		}
		setStorage(&crd, newVersion)
		if err := testClient.Update(ctx, &crd); err != nil {
			t.Fatalf("flip %s: %v", k.crdName, err)
		}
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{oldVersion, newVersion}) {
			t.Fatalf("%s storedVersions = %v after the flip, want [%s %s]", k.crdName, stored, oldVersion, newVersion)
		}
	}
	sentinel := newObject("Widget", newVersion, "mig-widget-0")
	eventually(t, func() error {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(sentinel), sentinel); err != nil {
			return err
		}
		was := sentinel.GetResourceVersion()
		if err := testClient.Patch(ctx, sentinel, client.RawPatch(types.MergePatchType, []byte("{}"))); err != nil {
			return err
		}
		if sentinel.GetResourceVersion() == was {
			return errors.New("sentinel not yet re-encoded")
		}
		return nil
	})

	// One pass: every other object is rewritten (resourceVersion bumps), the
	// sentinel is already at v2 and is left alone, the lists were
	// paginated, the CRDs are trimmed, and the metric counts the rewrites.
	before := resourceVersions(t)
	reader := &countingReader{Reader: testClient}
	m := testMigrator(reader, testClient)
	m.PageSize = 2
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	after := resourceVersions(t)
	for name, rv := range before {
		if name == "Widget/mig-widget-0" {
			if after[name] != rv {
				t.Errorf("%s was already at %s and must not be rewritten (rv %s -> %s)", name, newVersion, rv, after[name])
			}
			continue
		}
		if after[name] == rv {
			t.Errorf("%s not rewritten (rv still %s)", name, rv)
		}
	}
	if got := reader.lists.Load(); got < 3 {
		t.Errorf("Widget list with PageSize 2 over %d objects made %d List calls, want pagination", widgets, got)
	}
	for _, k := range testKinds {
		if stored := storedVersions(t, k.crdName); !slices.Equal(stored, []string{newVersion}) {
			t.Errorf("%s storedVersions = %v after migration, want [%s]", k.crdName, stored, newVersion)
		}
	}
	if got := testutil.ToFloat64(migratedObjects.WithLabelValues("Widget")); got != widgets-1 {
		t.Errorf("kaalm_storage_migrated_objects_total{kind=Widget} = %v, want %d", got, widgets-1)
	}
	if got := testutil.ToFloat64(migratedObjects.WithLabelValues("Gadget")); got != 1 {
		t.Errorf("kaalm_storage_migrated_objects_total{kind=Gadget} = %v, want 1", got)
	}

	// A second pass is a no-op: every CRD is skipped, nothing is written.
	reader.lists.Store(0)
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := reader.lists.Load(); got != 0 {
		t.Errorf("second pass listed %d times, want 0 (every CRD already at [%s])", got, newVersion)
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

// TestMigrateAttemptsEveryKind proves one failing kind does not stop the
// pass: the other five are migrated and trimmed, the failed kind keeps both
// stored versions, and the error names it.
func TestMigrateAttemptsEveryKind(t *testing.T) {
	const failing = "toolproviders.kaalm.io"
	var objs []client.Object
	for _, k := range kinds {
		crd := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: k.crdName},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: kaalmv1beta1.GroupVersion.Group,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{Name: oldVersion, Served: true},
					{Name: StorageVersion, Served: true, Storage: true},
				},
			},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: []string{oldVersion, StorageVersion}},
		}
		objs = append(objs, crd)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).
		WithStatusSubresource(&apiextensionsv1.CustomResourceDefinition{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "ToolProviderList" {
					return errors.New("list refused")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	err := (&Migrator{Reader: c, Client: c}).Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), failing) {
		t.Fatalf("Migrate: err = %v, want an error naming %s", err, failing)
	}
	for _, k := range kinds {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := c.Get(context.Background(), client.ObjectKey{Name: k.crdName}, &crd); err != nil {
			t.Fatal(err)
		}
		want := []string{StorageVersion}
		if k.crdName == failing {
			want = []string{oldVersion, StorageVersion}
		}
		if !slices.Equal(crd.Status.StoredVersions, want) {
			t.Errorf("%s storedVersions = %v, want %v", k.crdName, crd.Status.StoredVersions, want)
		}
	}
}

// TestStartRerunsWhileLeading proves a clean pass does not end the
// migrator: it runs the pass again every ResyncInterval until ctx ends.
func TestStartRerunsWhileLeading(t *testing.T) {
	var objs []client.Object
	for _, k := range kinds {
		objs = append(objs, &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: k.crdName},
			Status:     apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: []string{StorageVersion}},
		})
	}
	var gets atomic.Int32
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	m := &Migrator{Reader: c, Client: c, ResyncInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start returned %v, want nil on context end", err)
	}
	if passes := gets.Load() / int32(len(kinds)); passes < 3 {
		t.Fatalf("Start ran %d clean passes in 200ms with a 10ms resync, want several", passes)
	}
}

// TestSummaryLoggedOnFirstPassOnly proves the summary line appears after the
// first clean pass even when nothing moved, which is the startup
// confirmation operators and the e2e suite read, and that later no-op
// passes stay quiet.
func TestSummaryLoggedOnFirstPassOnly(t *testing.T) {
	var objs []client.Object
	for _, k := range kinds {
		objs = append(objs, &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: k.crdName},
			Status:     apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: []string{StorageVersion}},
		})
	}
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
	var summaries int
	var current any
	ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) {
		if strings.Contains(args, "storage-version migration pass complete") {
			summaries++
			current = args
		}
	}, funcr.Options{}))

	m := &Migrator{Reader: c, Client: c}
	for range 3 {
		if err := m.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if summaries != 1 {
		t.Fatalf("three no-op passes logged %d summaries, want 1 (the first)", summaries)
	}
	if !strings.Contains(current.(string), `"kindsAlreadyCurrent"=6`) {
		t.Errorf("summary = %v, want kindsAlreadyCurrent 6", current)
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
	for _, k := range testKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: testGroup, Version: newVersion, Kind: k.kind + "List"})
		if err := testClient.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			out[k.kind+"/"+o.GetName()] = o.GetResourceVersion()
		}
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
