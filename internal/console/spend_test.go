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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const spendBody = `{"providers":{
	"anthropic-shared":{"period":"2026-08","workloads":{"agent/support-assistant":"1.23","task/fix-42":"0.40","(unattributed)":"0.10"}},
	"backup-prov":{"period":"2026-08","workloads":{"agent/support-assistant":"0.05"}}}}`

func TestWorkloadSpendFromGateway(t *testing.T) {
	rows, err := workloadSpendFromGateway([]byte(spendBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Provider != "anthropic-shared" || rows[1].Provider != "backup-prov" {
		t.Fatalf("providers = %+v, want sorted", rows)
	}
	first := rows[0]
	if first.Period != "2026-08" || len(first.Rows) != 3 {
		t.Fatalf("first = %+v", first)
	}
	// Rows sorted by workload: "(unattributed)" < "agent/..." < "task/...".
	if first.Rows[0].Workload != "(unattributed)" || first.Rows[1].Workload != "agent/support-assistant" ||
		first.Rows[2].Workload != "task/fix-42" {
		t.Errorf("row order = %+v", first.Rows)
	}
	if first.Rows[1].SpentUSD != "1.23" {
		t.Errorf("spend = %+v", first.Rows[1])
	}

	if _, err := workloadSpendFromGateway([]byte("not json")); err == nil {
		t.Error("bad body must error")
	}
}

func TestAgentSpendRows(t *testing.T) {
	all, _ := workloadSpendFromGateway([]byte(spendBody))
	rows := agentSpendRows(all, "support-assistant")
	if len(rows) != 2 || rows[0].Provider != "anthropic-shared" || rows[0].SpentUSD != "1.23" ||
		rows[1].Provider != "backup-prov" || rows[1].SpentUSD != "0.05" {
		t.Errorf("agent rows = %+v", rows)
	}
	// Tasks and other agents never match.
	if got := agentSpendRows(all, "fix-42"); got != nil {
		t.Errorf("task name must not match an agent row: %+v", got)
	}
}

func TestAPI_SpendCarriesWorkloads(t *testing.T) {
	h := newAPIHarness(t)
	h.chat.spendStatus, h.chat.spendBody = 200, []byte(spendBody)

	resp := h.get(t, "/api/v1/namespaces/team-a/spend", "priya-token")
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Spend     []SpendRow      `json:"spend"`
		Workloads []WorkloadSpend `json:"workloads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Spend) != 1 {
		t.Errorf("namespace rows = %+v", out.Spend)
	}
	if len(out.Workloads) != 2 || out.Workloads[0].Rows[1].Workload != "agent/support-assistant" {
		t.Errorf("workloads = %+v", out.Workloads)
	}
}

func TestAPI_SpendDegradesWhenGatewayUnreachable(t *testing.T) {
	h := newAPIHarness(t)
	h.chat.spendErr = fmt.Errorf("gateway down")

	resp := h.get(t, "/api/v1/namespaces/team-a/spend", "priya-token")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("spend with a dead gateway = %d, want 200 (degrade, not dark)", resp.StatusCode)
	}
	var out struct {
		Spend     []SpendRow      `json:"spend"`
		Workloads []WorkloadSpend `json:"workloads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Spend) != 1 || out.Workloads != nil {
		t.Errorf("degraded response = %+v", out)
	}
}

func TestAPI_AgentDetailCarriesOwnSpend(t *testing.T) {
	h := newAPIHarness(t)
	h.chat.spendStatus, h.chat.spendBody = 200, []byte(spendBody)

	got := decode[AgentDetail](t, h.get(t, "/api/v1/namespaces/team-a/agents/support-assistant", "priya-token"))
	if len(got.Spend) != 2 || got.Spend[0].SpentUSD != "1.23" {
		t.Errorf("detail spend = %+v", got.Spend)
	}
}

func TestUI_WorkloadSpendRenders(t *testing.T) {
	h := newAPIHarness(t)
	h.chat.spendStatus, h.chat.spendBody = 200, []byte(spendBody)
	c := uiClient(t, h)
	login(t, h, c, "priya-token")

	_, body := page(t, c, h.srv.URL+"/ns/team-a")
	for _, want := range []string{"Spend by workload", "agent/support-assistant", "task/fix-42", "(unattributed)", "0.10"} {
		if !strings.Contains(body, want) {
			t.Errorf("namespace page missing %q", want)
		}
	}

	_, body = page(t, c, h.srv.URL+"/ns/team-a/agents/support-assistant")
	for _, want := range []string{"Spend this period", "anthropic-shared", "1.23", "backup-prov"} {
		if !strings.Contains(body, want) {
			t.Errorf("agent page missing %q", want)
		}
	}
}

func TestGatewayChatClient_WorkloadSpend(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"providers":{}}`))
	}))
	defer srv.Close()

	c := &GatewayChatClient{BaseURL: srv.URL, Insecure: true}
	status, body, err := c.WorkloadSpend(context.Background(), "team a")
	if err != nil || status != 200 {
		t.Fatalf("spend = %d, %v", status, err)
	}
	if gotPath != "/v1/spend" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "namespace="+url.QueryEscape("team a") {
		t.Errorf("query = %q (namespace must be escaped)", gotQuery)
	}
	if b, _ := io.ReadAll(strings.NewReader(string(body))); string(b) != `{"providers":{}}` {
		t.Errorf("body = %s", body)
	}
}
