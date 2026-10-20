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

	"github.com/win07xp/kaalm/internal/eventrec"
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
// per replica, over a rolling window. No etcd writes per request. A path's
// entry is dropped once all its observations age out of the window, so memory
// and the health response follow the channels with recent traffic. See
// docs/src/gateways/user/platform-adapters.md.
type ChannelHealthStore struct {
	window    time.Duration
	startedAt time.Time
	now       func() time.Time

	mu           sync.Mutex
	observations map[string]*pathObservations // key: channel path
	lastPrune    time.Time                    // last whole-map sweep, by record or Snapshot
}

// pathObservations is one channel path's record. The newest success is kept
// apart from the failures, so a flood of failures (anyone who knows a path
// can send badly authenticated requests) never pushes it out.
type pathObservations struct {
	success  *healthObservation
	failures []healthObservation
}

// maxChannelObservations is how many failures the store keeps per channel
// path: the newest ones. The newest success is kept apart and does not
// count.
const maxChannelObservations = 256

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
		observations: map[string]*pathObservations{},
	}
}

// RecordSuccess notes an authenticated, dispatched delivery.
func (c *ChannelHealthStore) RecordSuccess(path string) {
	c.record(path, healthObservation{success: true, reason: healthReasonWebhookReady, at: c.now()})
}

// RecordFailure notes a failed observation with its reason. The failure text
// is cut to eventrec.MaxNoteBytes, so a replica's memory and the health
// response stay bounded whatever text a caller passes (a spec-supplied name,
// an upstream error, a platform reply).
func (c *ChannelHealthStore) RecordFailure(path, reason, lastError string) {
	c.record(path, healthObservation{reason: reason, lastError: eventrec.Truncate(lastError, eventrec.MaxNoteBytes), at: c.now()})
}

func (c *ChannelHealthStore) record(path string, obs healthObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	// Sweep at most once per window, so memory stays bounded when nothing
	// polls Snapshot without walking the whole map on every request.
	if now.Sub(c.lastPrune) >= c.window {
		c.pruneLocked(now)
	}
	p := c.observations[path]
	if p == nil {
		p = &pathObservations{}
		c.observations[path] = p
	}
	cutoff := now.Add(-c.window)
	if p.success != nil && !p.success.at.After(cutoff) {
		p.success = nil
	}
	if obs.success {
		// Keep the newer one: at is read before the lock, so two successes
		// can arrive out of order.
		if p.success == nil || !obs.at.Before(p.success.at) {
			p.success = &obs
		}
		return
	}
	kept := p.failures[:0]
	for _, o := range p.failures {
		if o.at.After(cutoff) {
			kept = append(kept, o)
		}
	}
	// Bound the list so a hot channel cannot grow memory without limit. Trim
	// before the append, leaving room for the new observation.
	if len(kept) >= maxChannelObservations {
		kept = kept[len(kept)-(maxChannelObservations-1):]
	}
	p.failures = append(kept, obs)
}

// pruneLocked drops every path whose observations have all aged out of the
// window. A deleted or renamed channel never records again, so its entry has
// to be dropped by age, not by its own next record. Callers hold c.mu.
func (c *ChannelHealthStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-c.window)
	for path, p := range c.observations {
		if !inWindow(p, cutoff) {
			delete(c.observations, path)
		}
	}
	c.lastPrune = now
}

// inWindow reports whether the success or any failure is after cutoff, the
// same test Snapshot uses. Failures are appended roughly in time order, so
// the walk from the end usually stops at the last element.
func inWindow(p *pathObservations, cutoff time.Time) bool {
	if p.success != nil && p.success.at.After(cutoff) {
		return true
	}
	for i := len(p.failures) - 1; i >= 0; i-- {
		if p.failures[i].at.After(cutoff) {
			return true
		}
	}
	return false
}

// channelHealthEntry is the per-channel wire shape.
type channelHealthEntry struct {
	State     string  `json:"state"` // success | failure; a path with no in-window observation has no key
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
	now := c.now()
	c.pruneLocked(now)
	prefix := "/channels/" + namespace + "/"
	cutoff := now.Add(-c.window)
	for path, p := range c.observations {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		var lastSuccess, lastFailure *healthObservation
		if p.success != nil && p.success.at.After(cutoff) {
			lastSuccess = p.success
		}
		for i := range p.failures {
			o := &p.failures[i]
			if o.at.After(cutoff) && (lastFailure == nil || o.at.After(lastFailure.at)) {
				lastFailure = o
			}
		}
		var entry channelHealthEntry
		switch {
		case lastSuccess != nil:
			ts := lastSuccess.at.UTC().Format(time.RFC3339)
			entry = channelHealthEntry{State: "success", Reason: &lastSuccess.reason, Timestamp: &ts}
			if lastFailure != nil && lastFailure.lastError != "" {
				le := lastFailure.lastError
				entry.LastError = &le
			}
		default: // pruneLocked left only paths with an in-window observation
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
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		badRequest(w, "namespace query parameter is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.ChannelHealth.Snapshot(ns))
}
