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
	"math"
	"sync"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// RateLimiter enforces per-(namespace, model) request and token ceilings.
// Each configured limit is a cluster-wide ceiling; each replica divides it by
// the live replica count so the effective limit is replica-independent.
// Approximate by design: bursts may exceed the ceiling by up to one replica's
// share. The token ceiling is enforced after the fact: a request is admitted
// while the token bucket is above zero, and its settled usage is debited when
// the call ends (DebitTokens), so a large call blocks the next request rather
// than itself. See docs/src/gateways/llm/budgets-and-rate-limits.md.
type RateLimiter struct {
	// Replicas returns the live gateway replica count (>= 1). Injected so
	// tests need no informer.
	Replicas func() int
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket // key: namespace/model, or a prefixed key
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

// Allow reports whether an LLM request may proceed and, when it may not, the
// Retry-After in seconds. It checks the token bucket first (tokensPerMinute:
// admitted while above zero, nothing consumed), then consumes one request
// token (requestsPerMinute), so a request the token bucket refuses keeps its
// request token. An unset limit always allows.
func (r *RateLimiter) Allow(provider *kaalmv1beta1.ModelProvider, namespace, model string) (bool, int) {
	limits := provider.Spec.RateLimits
	r.mu.Lock()
	defer r.mu.Unlock()
	if limits.TokensPerMinute > 0 {
		perReplica := r.perReplica(limits.TokensPerMinute)
		b := r.refilled(tokenKey(namespace, model), perReplica, perReplica)
		if b.tokens <= 0 {
			return false, retryAfterSeconds(-b.tokens, perReplica)
		}
	}
	if limits.RequestsPerMinute > 0 {
		perReplica := r.perReplica(limits.RequestsPerMinute)
		b := r.refilled(namespace+"/"+model, perReplica, perReplica)
		if b.tokens < 1 {
			return false, retryAfterSeconds(1-b.tokens, perReplica)
		}
		b.tokens--
	}
	return true, 0
}

// DebitTokens subtracts a finished call's settled tokens from the
// (namespace, model) token bucket that admitted it. The bucket may go
// negative, down to minus one burst, so one huge call blocks the key for at
// most about one refill window. A provider with no tokensPerMinute limit
// debits nothing.
func (r *RateLimiter) DebitTokens(provider *kaalmv1beta1.ModelProvider, namespace, model string, tokens int64) {
	limit := provider.Spec.RateLimits.TokensPerMinute
	if limit <= 0 || tokens <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	perReplica := r.perReplica(limit)
	b := r.refilled(tokenKey(namespace, model), perReplica, perReplica)
	b.tokens -= float64(tokens)
	if b.tokens < -perReplica {
		b.tokens = -perReplica
	}
}

// tokenKey is the token bucket's key for (namespace, model), under a prefix
// no namespace name can produce, so it never collides with the request
// bucket of the same pair.
func tokenKey(namespace, model string) string { return "tpm:" + namespace + "/" + model }

// retryAfterSeconds is the whole seconds until a bucket refilling at
// perMinute gains deficit tokens, at least 1.
func retryAfterSeconds(deficit, perMinute float64) int {
	secs := int(math.Ceil(deficit / perMinute * 60))
	if secs < 1 {
		return 1
	}
	return secs
}

// AllowTool is the brokered-call analog, keyed per (namespace, ToolProvider)
// under a prefix no namespace name can produce (":" is not a DNS label
// character), so tool buckets never collide with LLM model buckets.
func (r *RateLimiter) AllowTool(tp *kaalmv1beta1.ToolProvider, namespace string) bool {
	return r.allow(tp.Spec.RateLimits.RequestsPerMinute, "mcp:"+namespace+"/"+tp.Name)
}

// The heartbeat cap: each replica allows each Agent heartbeatPerMinute
// heartbeats (2 per second) with a burst of heartbeatBurst. The cap is per
// replica, not divided by the replica count: it bounds the work one agent can
// cause, and a timer-driven heartbeat sits far below it.
const (
	heartbeatPerMinute = 120
	heartbeatBurst     = 5
)

// AllowHeartbeat is the /v1/agent/heartbeat analog, keyed per (namespace,
// Agent) under a prefix no namespace name can produce, so heartbeat buckets
// never collide with model or tool buckets.
func (r *RateLimiter) AllowHeartbeat(namespace, agent string) bool {
	return r.take("hb:"+namespace+"/"+agent, heartbeatPerMinute, heartbeatBurst)
}

func (r *RateLimiter) allow(limit int32, key string) bool {
	if limit <= 0 {
		return true
	}
	perReplica := r.perReplica(limit)
	return r.take(key, perReplica, perReplica)
}

// perReplica divides a cluster-wide ceiling by the live replica count.
func (r *RateLimiter) perReplica(limit int32) float64 {
	replicas := r.Replicas()
	if replicas < 1 {
		replicas = 1
	}
	return float64(limit) / float64(replicas)
}

// take consumes one token from key's bucket, which refills at perMinute and
// holds at most burst tokens.
func (r *RateLimiter) take(key string, perMinute, burst float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.refilled(key, perMinute, burst)
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// refilled returns key's bucket, created full when missing, after adding
// the tokens earned since its last refill (capped at the burst). The caller
// holds r.mu.
func (r *RateLimiter) refilled(key string, perMinute, burst float64) *tokenBucket {
	now := r.now()
	b := r.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: burst, lastRefill: now, perMinute: perMinute}
		r.buckets[key] = b
	}
	elapsed := now.Sub(b.lastRefill).Minutes()
	if elapsed > 0 {
		b.tokens += elapsed * perMinute
		b.lastRefill = now
	}
	b.perMinute = perMinute
	if b.tokens > burst {
		b.tokens = burst
	}
	return b
}
