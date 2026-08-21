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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestPhaseCollector_CountsByPhaseAndConditions(t *testing.T) {
	agent := func(ns, name string, phase kaalmv1alpha1.AgentPhase) *kaalmv1alpha1.Agent {
		return &kaalmv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Status:     kaalmv1alpha1.AgentStatus{Phase: phase},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		agent("team-a", "a1", kaalmv1alpha1.AgentRunning),
		agent("team-a", "a2", kaalmv1alpha1.AgentRunning),
		agent("team-a", "a3", kaalmv1alpha1.AgentHibernated),
		agent("team-b", "fresh", ""), // not yet reconciled: counted as Pending
		&kaalmv1alpha1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "t1"},
			Status:     kaalmv1alpha1.AgentTaskStatus{Phase: kaalmv1alpha1.TaskSucceeded},
		},
		&kaalmv1alpha1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "c1"},
			Status: kaalmv1alpha1.AgentChannelStatus{
				Phase: kaalmv1alpha1.ChannelActive,
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Bound"},
					{Type: "PlatformConnected", Status: metav1.ConditionFalse, Reason: "WebhookAuthFailed"},
				},
			},
		},
		&kaalmv1alpha1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "c2"},
			Status:     kaalmv1alpha1.AgentChannelStatus{Phase: kaalmv1alpha1.ChannelDegraded},
		},
	).Build()

	expected := `
# HELP kaalm_agents Agents by phase and namespace.
# TYPE kaalm_agents gauge
kaalm_agents{namespace="team-a",phase="Hibernated"} 1
kaalm_agents{namespace="team-a",phase="Running"} 2
kaalm_agents{namespace="team-b",phase="Pending"} 1
# HELP kaalm_channels AgentChannels by namespace, phase, and the Ready and PlatformConnected conditions.
# TYPE kaalm_channels gauge
kaalm_channels{namespace="team-a",phase="Active",platform_connected="false",ready="true"} 1
kaalm_channels{namespace="team-a",phase="Degraded",platform_connected="unknown",ready="unknown"} 1
# HELP kaalm_tasks AgentTasks by phase and namespace.
# TYPE kaalm_tasks gauge
kaalm_tasks{namespace="team-a",phase="Succeeded"} 1
`
	if err := testutil.CollectAndCompare(&PhaseCollector{Reader: cl}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestPhaseCollector_ListFailureReportsNothing(t *testing.T) {
	c := &PhaseCollector{Reader: newErrListClient(t)}
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("collected %d series from a failing reader, want 0", n)
	}
}

// TestControllerCatalog_EveryDocumentedMetricIsRegistered pins the
// observability page's aggregated catalog (the spec) to the registry: every
// row sourced to the controller must be a registered name. This is the
// check that would have caught the phase gauges documented in v0.1 and
// implemented in v0.5.
func TestControllerCatalog_EveryDocumentedMetricIsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(hibernationsTotal, wakesTotal, providerBudgetCanonical,
		&PhaseCollector{Reader: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()})
	for _, name := range catalogMetrics(t, "Controller") {
		assertRegistered(t, reg, name)
	}
}

// catalogMetrics reads the metric names of one Source column value from the
// aggregated catalog table in docs/src/operations/observability.md.
func catalogMetrics(t *testing.T, source string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "src", "operations", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "| "+source+" |") {
			continue
		}
		cells := strings.Split(line, "|")
		name := strings.Trim(strings.TrimSpace(cells[2]), "`")
		if strings.HasPrefix(name, "kaalm_") { // skip the Endpoints table's port rows
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("no catalog rows for source %q", source)
	}
	return names
}

// assertRegistered proves a name is taken in reg by trying to register a
// probe under it: an already-registered descriptor (same or different shape)
// makes Register fail, so success means the catalog name is missing.
func assertRegistered(t *testing.T, reg *prometheus.Registry, name string) {
	t.Helper()
	probe := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "catalog probe"})
	if err := reg.Register(probe); err == nil {
		reg.Unregister(probe)
		t.Errorf("catalog metric %s is documented but not registered", name)
	}
}
