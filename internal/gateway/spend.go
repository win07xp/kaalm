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

package gateway

import (
	"encoding/json"
	"net/http"
)

// spendResponse is the GET /v1/spend wire shape
// (docs/src/gateways/api/internal-endpoints.md).
type spendResponse struct {
	Providers map[string]WorkloadProviderSpend `json:"providers"`
}

// handleSpend serves the per-workload spend view for one namespace: for each
// provider, the current period and USD per workload (agent/{name},
// task/{name}, or the unattributed bucket). Every replica holds the folded
// union of its own live counters and every peer's latest partial, so any
// single replica answers authoritatively to within one publish interval.
// The console SAN check has already run (ConsolePaths).
func (s *Server) handleSpend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		badRequest(w, "GET required")
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		badRequest(w, "namespace query parameter is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(spendResponse{Providers: s.Budget.WorkloadSpend(namespace)})
}
