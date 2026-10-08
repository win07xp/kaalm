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
// share. A request ceiling below the replica count still gives each replica's
// LLM request bucket and tool bucket room for one request or call, refilled at
// the per-replica share, so a cluster-wide burst can admit up to one request
// per replica (up to number_of_replicas requests at once) while the long-run
// rate stays at the ceiling. It does not cap concurrency. The token ceiling is
// enforced after the fact: a request is admitted while the token bucket is
// above zero, and its settled usage is debited when the call ends
// (DebitTokens), so a large call blocks the next request rather than itself.
// See docs/src/gateways/llm/budgets-and-rate-limits.md.
type RateLimiter struct {
	// Replicas returns the count of gateway replicas that take new traffic
	// (>= 1), the divisor of each limit. Injected so tests need no informer.
	Replicas func() int
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket // key: namespace/model, or a prefixed key
}

// tokenBucket is one bucket's level and the time it was last refilled. It
// stores no rate or burst: each refilled call passes the current
// per-replica share, so a change in the ceiling or the replica count takes
// effect on the bucket's next refill.
type tokenBucket struct {
	tokens     float64
	lastRefill time.Time
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
// request token. An unset limit always allows. The request bucket holds at
// least one request, so a ceiling below the replica count still admits one
// request per refill on each replica.
func (r *RateLimiter) Allow(provider *kaalmv1beta1.ModelProvider, namespace, model string) (bool, int) {
	limits := provider.Spec.RateLimits
	if limits.TokensPerMinute <= 0 && limits.RequestsPerMinute <= 0 {
		return true, 0
	}
	// Read the replica count before taking the lock: Replicas lists Pods from
	// the cache and can block, and must not stall every other limiter call.
	replicas := r.replicas()
	tokenShare := perReplica(limits.TokensPerMinute, replicas)
	requestShare := perReplica(limits.RequestsPerMinute, replicas)

	r.mu.Lock()
	defer r.mu.Unlock()
	if limits.TokensPerMinute > 0 {
		b := r.refilled(tokenKey(namespace, model), tokenShare, tokenShare)
		if b.tokens <= 0 {
			return false, retryAfterSeconds(-b.tokens, tokenShare)
		}
	}
	if limits.RequestsPerMinute > 0 {
		b := r.refilled(namespace+"/"+model, requestShare, requestBurst(requestShare))
		if b.tokens < 1 {
			return false, retryAfterSeconds(1-b.tokens, requestShare)
		}
		b.tokens--
	}
	return true, 0
}

// DebitTokens subtracts a finished call's settled tokens from the
// (namespace, model) token bucket that admitted it. The bucket may go
// negative, down to minus one burst at the current share, so one huge call
// blocks the key for at most about one refill window. When the share later
// shrinks (more replicas or a lower ceiling), refilled re-clamps the older
// debt, so the bound holds across those changes. A provider with no
// tokensPerMinute limit debits nothing.
func (r *RateLimiter) DebitTokens(provider *kaalmv1beta1.ModelProvider, namespace, model string, tokens int64) {
	limit := provider.Spec.RateLimits.TokensPerMinute
	if limit <= 0 || tokens <= 0 {
		return
	}
	share := perReplica(limit, r.replicas()) // outside the lock, as in Allow
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.refilled(tokenKey(namespace, model), share, share)
	b.tokens -= float64(tokens)
	if b.tokens < -share {
		b.tokens = -share
	}
}

// tokenKey is the token bucket's key for (namespace, model), under a prefix
// no namespace name can produce, so it never collides with the request
// bucket of the same pair.
func tokenKey(namespace, model string) string { return "tpm:" + namespace + "/" + model }

// retryAfterSeconds is the whole seconds until a bucket refilling at
// perMinute gains deficit tokens, at least 1. The wait is rounded to the
// nearest nanosecond, the clock's resolution, before the ceiling is taken,
// so float error in an exact wait (30.000000000000007s) does not add a
// second, while a real fraction of a second still rounds up.
func retryAfterSeconds(deficit, perMinute float64) int {
	wait := time.Duration(math.Round(deficit / perMinute * float64(time.Minute)))
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// AllowTool reports whether a brokered call may proceed and, when it may not,
// the Retry-After in seconds until the bucket holds a call again, computed as
// for the LLM request bucket. It is keyed per (namespace, ToolProvider)
// under a prefix no namespace name can produce (":" is not a DNS label
// character), so tool buckets never collide with LLM model buckets. Its
// bucket holds at least one call, as the LLM request bucket does, so a
// ceiling below the replica count still admits one call per refill on each
// replica.
func (r *RateLimiter) AllowTool(tp *kaalmv1beta1.ToolProvider, namespace string) (bool, int) {
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
	ok, _ := r.take("hb:"+namespace+"/"+agent, heartbeatPerMinute, heartbeatBurst)
	return ok
}

func (r *RateLimiter) allow(limit int32, key string) (bool, int) {
	if limit <= 0 {
		return true, 0
	}
	share := perReplica(limit, r.replicas())
	return r.take(key, share, requestBurst(share))
}

// replicas is the live replica count, at least 1. It calls Replicas, which
// can block, so callers invoke it before taking r.mu.
func (r *RateLimiter) replicas() int {
	if n := r.Replicas(); n > 1 {
		return n
	}
	return 1
}

// perReplica divides a cluster-wide ceiling by the replica count.
func perReplica(limit int32, replicas int) float64 {
	return float64(limit) / float64(replicas)
}

// requestBurst is the burst of a bucket that counts whole requests or calls:
// the per-replica share, but room for at least one, so a share below one
// still admits one per refill. The refill rate stays at the share, so the
// long-run rate stays at the ceiling.
func requestBurst(share float64) float64 { return math.Max(share, 1) }

// take consumes one token from key's bucket, which refills at perMinute and
// holds at most burst tokens, and returns (true, 0). When the bucket holds
// less than one token it consumes nothing and returns false with the whole
// seconds until it holds one, at least 1.
func (r *RateLimiter) take(key string, perMinute, burst float64) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.refilled(key, perMinute, burst)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, retryAfterSeconds(1-b.tokens, perMinute)
}

// refilled returns key's bucket, created full when missing. It first raises
// a debt deeper than one burst to minus one burst, so a debt taken at a
// larger share (before the replica count grew or the ceiling dropped) blocks
// for at most one refill window at the current share. Then it adds the
// tokens earned since the last refill, capped at the burst. The caller holds
// r.mu.
func (r *RateLimiter) refilled(key string, perMinute, burst float64) *tokenBucket {
	now := r.now()
	b := r.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: burst, lastRefill: now}
		r.buckets[key] = b
	}
	// Clamp before the refill, so the debt counts as if it had been clamped
	// at the current share at the last refill.
	if b.tokens < -burst {
		b.tokens = -burst
	}
	elapsed := now.Sub(b.lastRefill).Minutes()
	if elapsed > 0 {
		b.tokens += elapsed * perMinute
		b.lastRefill = now
	}
	if b.tokens > burst {
		b.tokens = burst
	}
	return b
}
