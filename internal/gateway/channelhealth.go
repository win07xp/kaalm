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
	"strings"
	"sync"
	"time"
)

// Channel health observation reasons.
const (
	healthReasonWebhookReady     = "WebhookReady"
	healthReasonAuthFailed       = "WebhookAuthFailed"
	healthReasonAgentNotReady    = "AgentNotReady"
	healthReasonDispatchFailed   = "DispatchFailed"
	healthReasonCallbackInvalid  = "CallbackInvalid"
	healthReasonCallbackRejected = "CallbackRejected"
)

// ChannelHealthStore keeps per-channel-path delivery observations in memory,
// per replica, over a rolling window. No etcd writes per request. See
// docs/src/gateways/user/platform-adapters.md.
type ChannelHealthStore struct {
	window    time.Duration
	startedAt time.Time
	now       func() time.Time

	mu           sync.Mutex
	observations map[string][]healthObservation // key: channel path
}

type healthObservation struct {
	success   bool
	reason    string
	lastError string
	at        time.Time
}

// NewChannelHealthStore builds a store with the given rolling window.
func NewChannelHealthStore(window time.Duration) *ChannelHealthStore {
	if window == 0 {
		window = 5 * time.Minute
	}
	return &ChannelHealthStore{
		window: window, startedAt: time.Now(), now: time.Now,
		observations: map[string][]healthObservation{},
	}
}

// RecordSuccess notes an authenticated, dispatched delivery.
func (c *ChannelHealthStore) RecordSuccess(path string) {
	c.record(path, healthObservation{success: true, reason: healthReasonWebhookReady, at: c.now()})
}

// RecordFailure notes a failed observation with its reason.
func (c *ChannelHealthStore) RecordFailure(path, reason, lastError string) {
	c.record(path, healthObservation{reason: reason, lastError: lastError, at: c.now()})
}

func (c *ChannelHealthStore) record(path string, obs healthObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.observations[path]
	cutoff := c.now().Add(-c.window)
	kept := list[:0]
	for _, o := range list {
		if o.at.After(cutoff) {
			kept = append(kept, o)
		}
	}
	// Bound the list so a hot channel cannot grow memory without limit.
	if len(kept) > 256 {
		kept = kept[len(kept)-256:]
	}
	c.observations[path] = append(kept, obs)
}

// channelHealthEntry is the per-channel wire shape.
type channelHealthEntry struct {
	State     string  `json:"state"` // success | failure | empty
	Reason    *string `json:"reason"`
	Timestamp *string `json:"timestamp"`
	LastError *string `json:"lastError"`
}

type channelHealthResponse struct {
	WindowSeconds    int                           `json:"windowSeconds"`
	ReplicaStartedAt time.Time                     `json:"replicaStartedAt"`
	Channels         map[string]channelHealthEntry `json:"channels"`
}

// Snapshot reduces the in-window observations for one namespace's channels
// (paths carry the /channels/{namespace}/ prefix).
func (c *ChannelHealthStore) Snapshot(namespace string) channelHealthResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := channelHealthResponse{
		WindowSeconds:    int(c.window.Seconds()),
		ReplicaStartedAt: c.startedAt,
		Channels:         map[string]channelHealthEntry{},
	}
	prefix := "/channels/" + namespace + "/"
	cutoff := c.now().Add(-c.window)
	for path, list := range c.observations {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		var lastSuccess, lastFailure *healthObservation
		for i := range list {
			o := &list[i]
			if !o.at.After(cutoff) {
				continue
			}
			if o.success {
				if lastSuccess == nil || o.at.After(lastSuccess.at) {
					lastSuccess = o
				}
			} else if lastFailure == nil || o.at.After(lastFailure.at) {
				lastFailure = o
			}
		}
		entry := channelHealthEntry{State: "empty"}
		switch {
		case lastSuccess != nil:
			ts := lastSuccess.at.UTC().Format(time.RFC3339)
			entry = channelHealthEntry{State: "success", Reason: &lastSuccess.reason, Timestamp: &ts}
			if lastFailure != nil && lastFailure.lastError != "" {
				le := lastFailure.lastError
				entry.LastError = &le
			}
		case lastFailure != nil:
			ts := lastFailure.at.UTC().Format(time.RFC3339)
			le := lastFailure.lastError
			entry = channelHealthEntry{State: "failure", Reason: &lastFailure.reason, Timestamp: &ts, LastError: &le}
		}
		resp.Channels[path] = entry
	}
	return resp
}

// handleChannelsHealth serves GET /v1/channels/health?namespace= (controller
// SAN enforced by the middleware).
func (s *Server) handleChannelsHealth(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		badRequest(w, "namespace query parameter is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.ChannelHealth.Snapshot(ns))
}
