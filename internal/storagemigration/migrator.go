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

// Package storagemigration holds the controller's storage-version migrator:
// the one-shot pass at controller start that rewrites every Kaalm custom
// resource at the v1beta1 storage version and trims each CRD's
// status.storedVersions to ["v1beta1"], as the API Versioning and
// Deprecation chapter of the design book specifies.
package storagemigration

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// StorageVersion is the version every object is rewritten at and the only
// entry a migrated CRD lists in status.storedVersions: the hub. The test
// ties it to kaalmv1beta1.GroupVersion.Version.
const StorageVersion = "v1beta1"

const (
	defaultPageSize   = 500
	initialBackoff    = time.Second
	defaultMaxBackoff = 5 * time.Minute
)

// entry is one CRD the migrator owns.
type entry struct {
	// crdName is the CRD's metadata.name (plural.group).
	crdName string
	// kind is the kind name, used for the list GVK and the metric label.
	kind string
}

// kinds lists the six Kaalm CRDs. The order is the dependency order the
// chart installs them in; nothing depends on it here.
var kinds = []entry{
	{crdName: "agentclasses.kaalm.io", kind: "AgentClass"},
	{crdName: "modelproviders.kaalm.io", kind: "ModelProvider"},
	{crdName: "toolproviders.kaalm.io", kind: "ToolProvider"},
	{crdName: "agents.kaalm.io", kind: "Agent"},
	{crdName: "agenttasks.kaalm.io", kind: "AgentTask"},
	{crdName: "agentchannels.kaalm.io", kind: "AgentChannel"},
}

// migratedObjects counts objects the migrator actually moved: those whose
// empty patch produced a write, which the apiserver does only when the
// stored bytes were not already at the storage version.
var migratedObjects = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kaalm_storage_migrated_objects_total",
	Help: "Custom resources rewritten at the v1beta1 storage version by the migrator, by kind.",
}, []string{"kind"})

func init() {
	metrics.Registry.MustRegister(migratedObjects)
	for _, k := range kinds {
		migratedObjects.WithLabelValues(k.kind).Add(0)
	}
}

// The migrator reads each CRD by name and patches its status; it never
// lists CRDs and never writes a CRD's spec.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=agentclasses.kaalm.io;modelproviders.kaalm.io;toolproviders.kaalm.io;agents.kaalm.io;agenttasks.kaalm.io;agentchannels.kaalm.io,verbs=get
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions/status,resourceNames=agentclasses.kaalm.io;modelproviders.kaalm.io;toolproviders.kaalm.io;agents.kaalm.io;agenttasks.kaalm.io;agentchannels.kaalm.io,verbs=patch

// Migrator is a manager Runnable that runs one migration pass when the
// controller becomes leader and retries it until it succeeds. It needs no
// state between runs: a CRD already at ["v1beta1"] is skipped, and an
// object already stored at v1beta1 is a no-op write.
//
// The client's scheme must know apiextensions.k8s.io/v1; the Kaalm kinds
// are handled as unstructured objects.
type Migrator struct {
	// Reader serves the CRD gets and the paginated object lists. Give it
	// the manager's API reader: the lists must honor pagination, which the
	// cache does not, and a CRD get must not start a cluster-wide informer.
	Reader client.Reader
	// Client issues the object patches and the CRD status patch.
	Client client.Client
	// PageSize bounds each list page. Zero means 500.
	PageSize int64
	// MaxBackoff caps the retry delay between failed passes. Zero means
	// five minutes.
	MaxBackoff time.Duration
}

// NeedLeaderElection makes the manager run the migrator on the leader
// only: it is a write burst, and one writer is enough.
func (m *Migrator) NeedLeaderElection() bool { return true }

// Start runs Migrate until it succeeds or ctx ends, with exponential
// backoff between attempts. It returns nil in both cases: a migration that
// cannot complete must not stop the controller from reconciling, so a
// failure is a logged, retried condition rather than a manager error.
func (m *Migrator) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("storage-migrator")
	maxBackoff := m.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	backoff := initialBackoff
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	for {
		err := m.Migrate(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Error(err, "storage-version migration failed, retrying", "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Migrate runs one pass over the six CRDs and returns the first error. A
// pass is idempotent, so the caller may simply run it again.
func (m *Migrator) Migrate(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("storage-migrator")
	for _, k := range kinds {
		if err := m.migrateKind(ctx, log, k); err != nil {
			return fmt.Errorf("%s: %w", k.crdName, err)
		}
	}
	return nil
}

// migrateKind moves one CRD: skip when storedVersions is already
// ["v1beta1"], refuse when v1beta1 is not the CRD's storage version (the
// chart's CRDs were not applied, and trimming storedVersions would lie),
// otherwise rewrite every object and trim.
func (m *Migrator) migrateKind(ctx context.Context, log logr.Logger, k entry) error {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := m.Reader.Get(ctx, client.ObjectKey{Name: k.crdName}, &crd); err != nil {
		return fmt.Errorf("get CRD: %w", err)
	}
	if migrated(crd.Status.StoredVersions) {
		return nil
	}
	if !storesAt(crd, StorageVersion) {
		return fmt.Errorf("storage version is not %s (storedVersions %v); apply the chart's CRDs first",
			StorageVersion, crd.Status.StoredVersions)
	}
	before := append([]string(nil), crd.Status.StoredVersions...)
	seen, moved, err := m.rewriteAll(ctx, crd.Spec.Group, k.kind)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"status":{"storedVersions":[%q]}}`, StorageVersion)
	if err := m.Client.Status().Patch(ctx, &crd, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return fmt.Errorf("trim storedVersions: %w", err)
	}
	migratedObjects.WithLabelValues(k.kind).Add(float64(moved))
	log.Info("storage version migrated", "crd", k.crdName, "objects", seen, "rewritten", moved,
		"storedVersionsBefore", before)
	return nil
}

// rewriteAll lists every object of the kind at the storage version, page by
// page, and issues an empty merge patch for each. The apiserver re-encodes
// an object on any write whose stored bytes differ from the new encoding,
// and a version change is such a difference, so the no-op write moves the
// object and bumps its resourceVersion; an object already at the storage
// version is left untouched, resourceVersion included. It returns the
// number of objects seen and the number actually rewritten.
func (m *Migrator) rewriteAll(ctx context.Context, group, kindName string) (seen, moved int, err error) {
	pageSize := m.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	listGVK := schema.GroupVersionKind{Group: group, Version: StorageVersion, Kind: kindName + "List"}
	token := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := m.Reader.List(ctx, list, client.Limit(pageSize), client.Continue(token)); err != nil {
			return seen, moved, fmt.Errorf("list %s at %s: %w", kindName, StorageVersion, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			was := obj.GetResourceVersion()
			if err := m.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte("{}"))); err != nil {
				return seen, moved, fmt.Errorf("rewrite %s %s/%s: %w", kindName, obj.GetNamespace(), obj.GetName(), err)
			}
			seen++
			if obj.GetResourceVersion() != was {
				moved++
			}
		}
		token = list.GetContinue()
		if token == "" {
			return seen, moved, nil
		}
	}
}

// migrated reports whether storedVersions is exactly ["v1beta1"].
func migrated(stored []string) bool {
	return len(stored) == 1 && stored[0] == StorageVersion
}

// storesAt reports whether version is the CRD's storage version.
func storesAt(crd apiextensionsv1.CustomResourceDefinition, version string) bool {
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			return v.Name == version
		}
	}
	return false
}
