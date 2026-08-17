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

// Package console implements the operator console: the JSON read API, the
// server-rendered pages over the same data objects, the TokenReview and
// SubjectAccessReview gate, and the test-chat client
// (docs/src/console/overview.md).
package console

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// The DTOs below are the read API's wire shapes and the exact objects the
// page templates render (the swap rule). They are console-owned summaries,
// never raw CRD objects: fields are added, never renamed or removed, within
// a minor series.

// FleetRow is one agent in the fleet view.
type FleetRow struct {
	Name             string     `json:"name"`
	Phase            string     `json:"phase"`
	Ready            bool       `json:"ready"`
	Class            string     `json:"class"`
	HibernatedAt     *time.Time `json:"hibernatedAt,omitempty"`
	LastActivityTime *time.Time `json:"lastActivityTime,omitempty"`
}

// ConditionRow is one status condition, summarized.
type ConditionRow struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// AgentDetail is one agent in full: the fleet row plus conditions, grants,
// and child-resource names. Spec fields the pages never need (image, env,
// handler contents) deliberately do not transit.
type AgentDetail struct {
	FleetRow
	Conditions []ConditionRow `json:"conditions"`
	Providers  []string       `json:"providers"`
	Tools      []ToolGrant    `json:"tools"`
	Endpoint   string         `json:"endpoint,omitempty"`
	PodName    string         `json:"podName,omitempty"`
	PVCName    string         `json:"pvcName,omitempty"`
}

// ToolGrant is one granted ToolProvider with its optional per-tool narrowing.
type ToolGrant struct {
	Provider string   `json:"provider"`
	Tools    []string `json:"tools,omitempty"`
}

// TaskRow is one task in the history view. Artifact names only, never
// values: task history is about lifecycle, not content.
type TaskRow struct {
	Name           string     `json:"name"`
	Phase          string     `json:"phase"`
	StartTime      *time.Time `json:"startTime,omitempty"`
	CompletionTime *time.Time `json:"completionTime,omitempty"`
	Retries        int32      `json:"retries"`
	ArtifactNames  []string   `json:"artifactNames,omitempty"`
}

// ChannelRow is one channel in the health view.
type ChannelRow struct {
	Name              string `json:"name"`
	Agent             string `json:"agent"`
	Phase             string `json:"phase"`
	Ready             string `json:"ready"`
	PlatformConnected string `json:"platformConnected"`
	Reason            string `json:"reason,omitempty"`
}

// SpendRow is one provider's budget usage for the namespace being viewed.
type SpendRow struct {
	Provider    string `json:"provider"`
	Period      string `json:"period,omitempty"`
	SpentUSD    string `json:"spentUSD"`
	CeilingUSD  string `json:"ceilingUSD,omitempty"`
	PercentUsed int32  `json:"percentUsed"`
	State       string `json:"state"`
}

func fleetRow(a *kaalmv1alpha1.Agent) FleetRow {
	return FleetRow{
		Name:             a.Name,
		Phase:            string(a.Status.Phase),
		Ready:            meta.IsStatusConditionTrue(a.Status.Conditions, "Ready"),
		Class:            a.Spec.AgentClassRef.Name,
		HibernatedAt:     metaTime(a.Status.HibernatedAt),
		LastActivityTime: metaTime(a.Status.LastActivityTime),
	}
}

func agentDetail(a *kaalmv1alpha1.Agent) AgentDetail {
	d := AgentDetail{
		FleetRow:   fleetRow(a),
		Conditions: conditionRows(a.Status.Conditions),
		Providers:  []string{},
		Tools:      []ToolGrant{},
		Endpoint:   a.Status.Endpoint,
		PodName:    a.Status.PodName,
		PVCName:    a.Status.PVCName,
	}
	for _, p := range a.Spec.Providers {
		d.Providers = append(d.Providers, p.ProviderRef.Name)
	}
	for _, tg := range a.Spec.Tools {
		d.Tools = append(d.Tools, ToolGrant{Provider: tg.ProviderRef.Name, Tools: tg.Tools})
	}
	return d
}

func taskRow(t *kaalmv1alpha1.AgentTask) TaskRow {
	row := TaskRow{
		Name:           t.Name,
		Phase:          string(t.Status.Phase),
		StartTime:      metaTime(t.Status.StartTime),
		CompletionTime: metaTime(t.Status.CompletionTime),
		Retries:        t.Status.Retries,
	}
	for _, a := range t.Spec.Artifacts {
		row.ArtifactNames = append(row.ArtifactNames, a.Name)
	}
	return row
}

func channelRow(c *kaalmv1alpha1.AgentChannel) ChannelRow {
	row := ChannelRow{
		Name:              c.Name,
		Agent:             c.Spec.AgentRef.Name,
		Phase:             string(c.Status.Phase),
		Ready:             conditionStatus(c.Status.Conditions, "Ready"),
		PlatformConnected: conditionStatus(c.Status.Conditions, "PlatformConnected"),
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, "PlatformConnected"); cond != nil {
		row.Reason = cond.Reason
	}
	return row
}

// spendRows extracts the budget rows belonging to one namespace from a
// cluster-scoped provider. A provider with no usage row for the namespace
// contributes nothing: spend is shown where it exists.
func spendRows(p *kaalmv1alpha1.ModelProvider, namespace string) []SpendRow {
	var rows []SpendRow
	for _, u := range p.Status.BudgetUsage {
		if u.Namespace != namespace {
			continue
		}
		row := SpendRow{
			Provider:    p.Name,
			Period:      u.Period,
			SpentUSD:    u.SpentUSD,
			PercentUsed: u.PercentUsed,
			State:       u.State,
		}
		row.CeilingUSD = p.Spec.Budget.PerNamespaceUSD
		rows = append(rows, row)
	}
	return rows
}

func conditionRows(conds []metav1.Condition) []ConditionRow {
	rows := make([]ConditionRow, 0, len(conds))
	for _, c := range conds {
		rows = append(rows, ConditionRow{
			Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return rows
}

// conditionStatus reports a condition's tri-state as a string; a condition
// that has never been set reads as Unknown.
func conditionStatus(conds []metav1.Condition, condType string) string {
	if c := meta.FindStatusCondition(conds, condType); c != nil {
		return string(c.Status)
	}
	return string(metav1.ConditionUnknown)
}

func metaTime(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := t.Time
	return &out
}
