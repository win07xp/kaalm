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
	"sync"
	"time"
)

// ActivityStore keeps per-agent last-activity timestamps in memory, per
// replica: no etcd writes per request. Two signal sources are tracked
// separately and both are always returned; the controller owns the
// activitySource filtering policy. See
// docs/src/gateways/user/activation-and-activity.md.
type ActivityStore struct {
	startedAt time.Time
	now       func() time.Time

	mu sync.Mutex
	// byNamespace[ns][agent] = timestamps
	byNamespace map[string]map[string]*activityEntry
}

type activityEntry struct {
	gatewayTraffic time.Time
	heartbeat      time.Time
}

// NewActivityStore stamps replicaStartedAt at construction.
func NewActivityStore() *ActivityStore {
	return &ActivityStore{startedAt: time.Now(), now: time.Now, byNamespace: map[string]map[string]*activityEntry{}}
}

func (s *ActivityStore) entry(ns, agent string) *activityEntry {
	agents, ok := s.byNamespace[ns]
	if !ok {
		agents = map[string]*activityEntry{}
		s.byNamespace[ns] = agents
	}
	e, ok := agents[agent]
	if !ok {
		e = &activityEntry{}
		agents[agent] = e
	}
	return e
}

// RecordTraffic notes gateway-observed traffic (LLM request or channel
// delivery) for an agent.
func (s *ActivityStore) RecordTraffic(ns, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(ns, agent).gatewayTraffic = s.now()
}

// RecordHeartbeat notes an agent-emitted heartbeat. Heartbeats are recorded
// unconditionally; per-Agent activitySource filtering is controller-side.
func (s *ActivityStore) RecordHeartbeat(ns, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(ns, agent).heartbeat = s.now()
}

// activityResponse is the GET /v1/activity wire shape.
type activityResponse struct {
	ReplicaStartedAt time.Time                 `json:"replicaStartedAt"`
	Agents           map[string]activitySource `json:"agents"`
}

type activitySource struct {
	GatewayTraffic *time.Time `json:"gatewayTraffic"`
	Heartbeat      *time.Time `json:"heartbeat"`
}

// Snapshot returns the wire response for one namespace. A null source means
// this replica has no record of that signal since its last restart.
func (s *ActivityStore) Snapshot(ns string) activityResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := activityResponse{ReplicaStartedAt: s.startedAt, Agents: map[string]activitySource{}}
	for agent, e := range s.byNamespace[ns] {
		var src activitySource
		if !e.gatewayTraffic.IsZero() {
			t := e.gatewayTraffic
			src.GatewayTraffic = &t
		}
		if !e.heartbeat.IsZero() {
			t := e.heartbeat
			src.Heartbeat = &t
		}
		resp.Agents[agent] = src
	}
	return resp
}

// handleActivity serves GET /v1/activity?namespace={ns} (controller-only path;
// auth enforced by the middleware).
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		badRequest(w, "namespace query parameter is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Activity.Snapshot(ns))
}

// handleHeartbeat serves POST /v1/agent/heartbeat. The middleware has already
// enforced mTLS and the Agent-only kind split; every heartbeat updates the
// in-memory store regardless of the Agent's activitySource setting.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	c := callerFrom(r.Context())
	s.Activity.RecordHeartbeat(c.Namespace, c.Workload.Name)
	w.WriteHeader(http.StatusOK)
}
