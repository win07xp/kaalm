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
	"sync"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// RateLimiter enforces per-(namespace, model) request ceilings. The configured
// limit is a cluster-wide ceiling; each replica divides it by the live replica
// count so the effective limit is replica-independent. Approximate by design:
// bursts may exceed the ceiling by up to one replica's share. See
// docs/src/gateways/llm/budgets-and-rate-limits.md.
type RateLimiter struct {
	// Replicas returns the live gateway replica count (>= 1). Injected so
	// tests need no informer.
	Replicas func() int
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket // key: namespace/model
}

type tokenBucket struct {
	tokens     float64
	lastRefill time.Time
	// perMinute is the per-replica ceiling this bucket was last sized for; a
	// change in the ceiling or replica count re-sizes on the next refill.
	perMinute float64
}

// NewRateLimiter builds a limiter. replicas defaults to 1 when nil.
func NewRateLimiter(replicas func() int) *RateLimiter {
	if replicas == nil {
		replicas = func() int { return 1 }
	}
	return &RateLimiter{Replicas: replicas, now: time.Now, buckets: map[string]*tokenBucket{}}
}

// Allow reports whether a request may proceed, consuming one token when it
// can. A provider with no requestsPerMinute limit always allows.
func (r *RateLimiter) Allow(provider *kaalmv1beta1.ModelProvider, namespace, model string) bool {
	return r.allow(provider.Spec.RateLimits.RequestsPerMinute, namespace+"/"+model)
}

// AllowTool is the brokered-call analog, keyed per (namespace, ToolProvider)
// under a prefix no namespace name can produce (":" is not a DNS label
// character), so tool buckets never collide with LLM model buckets.
func (r *RateLimiter) AllowTool(tp *kaalmv1beta1.ToolProvider, namespace string) bool {
	return r.allow(tp.Spec.RateLimits.RequestsPerMinute, "mcp:"+namespace+"/"+tp.Name)
}

func (r *RateLimiter) allow(limit int32, key string) bool {
	if limit <= 0 {
		return true
	}
	replicas := r.Replicas()
	if replicas < 1 {
		replicas = 1
	}
	perReplica := float64(limit) / float64(replicas)

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: perReplica, lastRefill: now, perMinute: perReplica}
		r.buckets[key] = b
	}

	// Refill proportional to elapsed time, capped at the per-replica ceiling.
	elapsed := now.Sub(b.lastRefill).Minutes()
	if elapsed > 0 {
		b.tokens += elapsed * perReplica
		b.lastRefill = now
	}
	b.perMinute = perReplica
	if b.tokens > perReplica {
		b.tokens = perReplica
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
