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

package console

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func seededData(t *testing.T) *Data {
	t.Helper()
	hibernated := metav1.NewTime(time.Date(2026, 8, 14, 2, 11, 9, 0, time.UTC))
	active := metav1.NewTime(time.Date(2026, 8, 13, 22, 41, 55, 0, time.UTC))
	earlier := metav1.NewTime(time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC))

	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
		&kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "support-assistant", Namespace: "team-a"},
			Spec: kaalmv1beta1.AgentSpec{
				AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "standard"},
				Providers: []kaalmv1beta1.AgentProviderReference{
					{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "anthropic-shared"}},
				},
				Tools: []kaalmv1beta1.AgentToolGrant{
					{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "search-tools"}, Tools: []string{"web_search"}},
				},
			},
			Status: kaalmv1beta1.AgentStatus{
				Phase:            kaalmv1beta1.AgentHibernated,
				HibernatedAt:     &hibernated,
				LastActivityTime: &active,
				PodName:          "support-assistant-0",
				PVCName:          "support-assistant-state",
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Hibernated", LastTransitionTime: hibernated},
				},
			},
		},
		&kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
			Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "sandboxed"}},
			Status: kaalmv1beta1.AgentStatus{
				Phase: kaalmv1beta1.AgentRunning,
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running", LastTransitionTime: active},
				},
			},
		},
		&kaalmv1beta1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"},
			Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "standard"}},
		},
		&kaalmv1beta1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Name: "older-task", Namespace: "team-a"},
			Spec: kaalmv1beta1.AgentTaskSpec{
				Artifacts: []kaalmv1beta1.AgentTaskArtifact{{Name: "summary"}, {Name: "report"}},
			},
			Status: kaalmv1beta1.AgentTaskStatus{
				Phase: kaalmv1beta1.AgentTaskPhase("Succeeded"), StartTime: &earlier,
				CompletionTime: &active, Retries: 1,
				ArtifactValues: map[string]string{"summary": "SENSITIVE OUTPUT"},
			},
		},
		&kaalmv1beta1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Name: "newer-task", Namespace: "team-a"},
			Status:     kaalmv1beta1.AgentTaskStatus{Phase: kaalmv1beta1.AgentTaskPhase("Running"), StartTime: &active},
		},
		&kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-a"},
			Spec:       kaalmv1beta1.AgentChannelSpec{AgentRef: kaalmv1beta1.LocalObjectReference{Name: "support-assistant"}},
			Status: kaalmv1beta1.AgentChannelStatus{
				Phase: kaalmv1beta1.AgentChannelPhase("Active"),
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "WebhookReady", LastTransitionTime: active},
				},
			},
		},
		&kaalmv1beta1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "anthropic-shared"},
			Spec: kaalmv1beta1.ModelProviderSpec{
				Budget: kaalmv1beta1.ModelProviderBudget{PerNamespaceUSD: "100.00", Period: "monthly"},
			},
			Status: kaalmv1beta1.ModelProviderStatus{
				BudgetUsage: []kaalmv1beta1.ModelProviderBudgetUsage{
					{Namespace: "team-a", Period: "monthly", SpentUSD: "12.34", PercentUsed: 12, State: "Normal"},
					{Namespace: "team-b", Period: "monthly", SpentUSD: "99.00", PercentUsed: 99, State: "Throttled"},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &Data{Reader: c}
}

func TestData_FleetMapsAndSorts(t *testing.T) {
	d := seededData(t)
	rows, err := d.Fleet(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "coder" || rows[1].Name != "support-assistant" {
		t.Fatalf("fleet = %+v, want coder then support-assistant", rows)
	}
	sup := rows[1]
	if sup.Phase != "Hibernated" || sup.Ready || sup.Class != "standard" {
		t.Errorf("row = %+v", sup)
	}
	if sup.HibernatedAt == nil || sup.LastActivityTime == nil {
		t.Error("timestamps must map")
	}
	if !rows[0].Ready {
		t.Error("coder has Ready=True and must map to ready true")
	}
}

func TestData_AgentDetail(t *testing.T) {
	d := seededData(t)
	detail, found, err := d.Agent(context.Background(), "team-a", "support-assistant")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if len(detail.Conditions) != 1 || detail.Conditions[0].Reason != "Hibernated" {
		t.Errorf("conditions = %+v", detail.Conditions)
	}
	if len(detail.Providers) != 1 || detail.Providers[0] != "anthropic-shared" {
		t.Errorf("providers = %v", detail.Providers)
	}
	if len(detail.Tools) != 1 || detail.Tools[0].Provider != "search-tools" || detail.Tools[0].Tools[0] != "web_search" {
		t.Errorf("tools = %+v", detail.Tools)
	}
	if detail.PodName != "support-assistant-0" || detail.PVCName != "support-assistant-state" {
		t.Errorf("child names = %q %q", detail.PodName, detail.PVCName)
	}

	if _, found, err := d.Agent(context.Background(), "team-a", "nope"); err != nil || found {
		t.Errorf("missing agent: found=%v err=%v", found, err)
	}
}

func TestData_TasksNewestFirstAndNamesOnly(t *testing.T) {
	d := seededData(t)
	rows, err := d.Tasks(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "newer-task" || rows[1].Name != "older-task" {
		t.Fatalf("tasks = %+v, want newest first", rows)
	}
	older := rows[1]
	if older.Retries != 1 || len(older.ArtifactNames) != 2 || older.ArtifactNames[0] != "summary" {
		t.Errorf("older task row = %+v", older)
	}
	// Artifact values are workload output and must never transit.
	for _, r := range rows {
		for _, n := range r.ArtifactNames {
			if n == "SENSITIVE OUTPUT" {
				t.Fatal("artifact values leaked into the task row")
			}
		}
	}
}

func TestData_ChannelsAndConditionTriState(t *testing.T) {
	d := seededData(t)
	rows, err := d.Channels(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("channels = %+v", rows)
	}
	ch := rows[0]
	if ch.Agent != "support-assistant" || ch.Ready != "True" {
		t.Errorf("row = %+v", ch)
	}
	// PlatformConnected was never set: tri-state reads Unknown, not empty.
	if ch.PlatformConnected != "Unknown" {
		t.Errorf("platformConnected = %q, want Unknown", ch.PlatformConnected)
	}
}

func TestData_SpendFiltersToNamespace(t *testing.T) {
	d := seededData(t)
	rows, err := d.Spend(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("spend = %+v, want only team-a's row", rows)
	}
	r := rows[0]
	if r.Provider != "anthropic-shared" || r.SpentUSD != "12.34" || r.CeilingUSD != "100.00" || r.PercentUsed != 12 {
		t.Errorf("row = %+v", r)
	}

	empty, err := d.Spend(context.Background(), "team-c")
	if err != nil || len(empty) != 0 {
		t.Errorf("no-usage namespace must return zero rows, got %+v", empty)
	}
}

func TestData_Namespaces(t *testing.T) {
	d := seededData(t)
	names, err := d.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "team-a" || names[1] != "team-b" {
		t.Errorf("namespaces = %v", names)
	}
}
