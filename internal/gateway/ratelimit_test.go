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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func rlProvider(rpm int32) *kaalmv1beta1.ModelProvider {
	return &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov"},
		Spec:       kaalmv1beta1.ModelProviderSpec{RateLimits: kaalmv1beta1.ModelProviderRateLimits{RequestsPerMinute: rpm}},
	}
}

// allowLLM is Allow without the Retry-After, for tests that count admissions.
func allowLLM(rl *RateLimiter, p *kaalmv1beta1.ModelProvider, namespace, model string) bool {
	ok, _ := rl.Allow(p, namespace, model)
	return ok
}

func rlTokenProvider(rpm, tpm int32) *kaalmv1beta1.ModelProvider {
	p := rlProvider(rpm)
	p.Spec.RateLimits.TokensPerMinute = tpm
	return p
}

// TestRateLimiter_TokenDebitBlocksNextRequest: a large debit drives the
// token bucket below zero, so the next request is refused with a
// Retry-After computed from the debt, and admitted again once the bucket
// refills above zero (#202).
func TestRateLimiter_TokenDebitBlocksNextRequest(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	if ok, _ := rl.Allow(p, "team-a", "m1"); !ok {
		t.Fatal("a full token bucket must admit")
	}
	rl.DebitTokens(p, "team-a", "m1", 1500) // 1000 - 1500 = -500
	ok, retry := rl.Allow(p, "team-a", "m1")
	if ok {
		t.Fatal("a bucket in debt must refuse the next request")
	}
	if retry != 30 { // 500 tokens at 1000/min is 30s
		t.Errorf("Retry-After = %d, want 30", retry)
	}
	// Another namespace and another model hold their own buckets.
	if !allowLLM(rl, p, "team-b", "m1") || !allowLLM(rl, p, "team-a", "m2") {
		t.Error("each (namespace, model) must have its own token bucket")
	}
	// At exactly zero the bucket still refuses, with the 1s floor.
	now = now.Add(30 * time.Second)
	ok, retry = rl.Allow(p, "team-a", "m1")
	if ok || retry != 1 {
		t.Errorf("at zero tokens: ok=%v retry=%d, want refused with 1", ok, retry)
	}
	now = now.Add(time.Second)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("a bucket refilled above zero must admit")
	}
}

// TestRateLimiter_TokenAdmitsAboveZero: admission needs the bucket above
// zero, not a whole token.
func TestRateLimiter_TokenAdmitsAboveZero(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	rl.DebitTokens(p, "team-a", "m1", 1000) // 0 left
	now = now.Add(30 * time.Millisecond)    // refills half a token
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("half a token left must admit (admission is tokens > 0)")
	}
}

// TestRateLimiter_TokenDebtClampedAtOneBurst: one huge call blocks for at
// most about one refill window.
func TestRateLimiter_TokenDebtClampedAtOneBurst(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	rl.DebitTokens(p, "team-a", "m1", 1_000_000)
	ok, retry := rl.Allow(p, "team-a", "m1")
	if ok || retry != 60 { // the debt clamps at -1000, a minute at 1000/min
		t.Errorf("after a huge debit: ok=%v retry=%d, want refused with 60", ok, retry)
	}
}

// TestRateLimiter_TokensDivideByReplicas: the token ceiling splits across
// live replicas like the request ceiling.
func TestRateLimiter_TokensDivideByReplicas(t *testing.T) {
	rl := NewRateLimiter(func() int { return 4 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 4000) // per replica 1000

	rl.DebitTokens(p, "team-a", "m1", 1500)
	ok, retry := rl.Allow(p, "team-a", "m1")
	if ok || retry != 30 {
		t.Errorf("per-replica 1000 after a 1500 debit: ok=%v retry=%d, want refused with 30", ok, retry)
	}
}

// TestRateLimiter_NoTokenLimitIgnoresDebits: an unset tokensPerMinute keeps
// the request-only behavior.
func TestRateLimiter_NoTokenLimitIgnoresDebits(t *testing.T) {
	rl := NewRateLimiter(nil)
	p := rlTokenProvider(0, 0)
	rl.DebitTokens(p, "team-a", "m1", 1_000_000)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("with no tokensPerMinute a debit must not block")
	}
}

// TestRateLimiter_TokenRefusalKeepsRequestToken: a request the token bucket
// refuses does not spend a request token.
func TestRateLimiter_TokenRefusalKeepsRequestToken(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(1, 60) // 1 request/min, 1 token/s

	rl.DebitTokens(p, "team-a", "m1", 61) // -1
	for i := 0; i < 3; i++ {
		if allowLLM(rl, p, "team-a", "m1") {
			t.Fatal("a bucket in debt must refuse")
		}
	}
	now = now.Add(2 * time.Second) // tokens back to +1; requests still full
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("the request token must survive the token refusals")
	}
}

// TestRateLimiter_RequestRetryAfter: a request refusal carries the time
// until the next request token.
func TestRateLimiter_RequestRetryAfter(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(2) // one request per 30s

	allowLLM(rl, p, "team-a", "m1")
	allowLLM(rl, p, "team-a", "m1")
	ok, retry := rl.Allow(p, "team-a", "m1")
	if ok || retry != 30 {
		t.Errorf("drained 2/min bucket: ok=%v retry=%d, want refused with 30", ok, retry)
	}
	now = now.Add(15 * time.Second)
	if _, retry = rl.Allow(p, "team-a", "m1"); retry != 15 {
		t.Errorf("half refilled: retry=%d, want 15", retry)
	}
}

func TestRateLimiter_UnlimitedAllows(t *testing.T) {
	rl := NewRateLimiter(nil)
	p := rlProvider(0)
	for i := 0; i < 100; i++ {
		if !allowLLM(rl, p, "team-a", "m1") {
			t.Fatal("a provider with no limit must always allow")
		}
	}
}

func TestRateLimiter_EnforcesCeiling(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(5)

	// The bucket starts full at the per-replica ceiling (5).
	allowed := 0
	for i := 0; i < 10; i++ {
		if allowLLM(rl, p, "team-a", "m1") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("allowed %d of 10, want 5 (the ceiling)", allowed)
	}
	// A different (namespace, model) has its own bucket.
	if !allowLLM(rl, p, "team-b", "m1") {
		t.Error("a different namespace must have a fresh bucket")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(60) // 1 token/sec

	for i := 0; i < 60; i++ {
		allowLLM(rl, p, "team-a", "m1")
	}
	if allowLLM(rl, p, "team-a", "m1") {
		t.Fatal("bucket should be empty after draining the ceiling")
	}
	// 2 seconds later, ~2 tokens have refilled.
	now = now.Add(2 * time.Second)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("first refilled token should be available")
	}
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("second refilled token should be available")
	}
}

func TestRateLimiter_DividesByReplicas(t *testing.T) {
	replicas := 4
	rl := NewRateLimiter(func() int { return replicas })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(8) // cluster ceiling 8, per-replica 2

	allowed := 0
	for i := 0; i < 10; i++ {
		if allowLLM(rl, p, "team-a", "m1") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("with 4 replicas each gets 8/4=2, allowed %d", allowed)
	}
}

func TestNewRateLimiter_DefaultReplicas(t *testing.T) {
	rl := NewRateLimiter(nil)
	if rl.Replicas() != 1 {
		t.Errorf("nil replicas must default to 1, got %d", rl.Replicas())
	}
	rl2 := NewRateLimiter(func() int { return 3 })
	if rl2.Replicas() != 3 {
		t.Errorf("explicit replicas = %d", rl2.Replicas())
	}
}

func TestAllowTool_BucketsAndNoLimit(t *testing.T) {
	rl := NewRateLimiter(nil)
	unlimited := &kaalmv1beta1.ToolProvider{}
	unlimited.Name = "open"
	for i := 0; i < 5; i++ {
		if !rl.AllowTool(unlimited, "team-a") {
			t.Fatal("a ToolProvider with no rate limit must always allow")
		}
	}

	tp := &kaalmv1beta1.ToolProvider{}
	tp.Name = "search"
	tp.Spec.RateLimits.RequestsPerMinute = 2
	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.AllowTool(tp, "team-a") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("allowed %d calls, want the 2-per-minute ceiling", allowed)
	}
	// A different namespace holds its own bucket. (Cross-plane collisions
	// are unreachable by construction: the "mcp:" prefix contains a
	// character no namespace name can, so no LLM key can equal a tool key.)
	if !rl.AllowTool(tp, "team-b") {
		t.Error("second namespace must carry its own bucket")
	}
}

func TestAllowHeartbeat_BurstAndRefill(t *testing.T) {
	// Replica count does not divide the heartbeat cap.
	rl := NewRateLimiter(func() int { return 4 })
	now := time.Now()
	rl.now = func() time.Time { return now }

	allowed := 0
	for i := 0; i < 20; i++ {
		if rl.AllowHeartbeat("team-a", "sup") {
			allowed++
		}
	}
	if allowed != heartbeatBurst {
		t.Errorf("allowed %d of 20, want the burst %d", allowed, heartbeatBurst)
	}
	// Another agent, and the same name in another namespace, have their own buckets.
	if !rl.AllowHeartbeat("team-a", "other") || !rl.AllowHeartbeat("team-b", "sup") {
		t.Error("each agent must have its own bucket")
	}
	// Half a second refills one token at 2 per second.
	now = now.Add(500 * time.Millisecond)
	if !rl.AllowHeartbeat("team-a", "sup") {
		t.Error("one token must refill after 500ms")
	}
	if rl.AllowHeartbeat("team-a", "sup") {
		t.Error("only one token must refill after 500ms")
	}
}

// TestRateLimiter_FewerRequestsThanReplicas: when requestsPerMinute is below
// the replica count, each replica's share is below one request, yet the
// request bucket still holds one whole request, so the key is admitted once
// per refill instead of refused forever (#202).
func TestRateLimiter_FewerRequestsThanReplicas(t *testing.T) {
	rl := NewRateLimiter(func() int { return 3 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(2) // 2/3 request per minute per replica

	if !allowLLM(rl, p, "team-a", "m1") {
		t.Fatal("a fresh bucket must admit one request even when the share is below 1")
	}
	ok, retry := rl.Allow(p, "team-a", "m1")
	if ok || retry != 90 { // 1 request at 2/3 per minute is 90s
		t.Errorf("drained bucket: ok=%v retry=%d, want refused with 90", ok, retry)
	}
	now = now.Add(90 * time.Second)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("a bucket refilled to one request must admit")
	}
}

// blockingReplicas returns a Replicas func whose first call blocks until
// release is closed, and a channel closed when that first call starts.
func blockingReplicas() (replicas func() int, entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	replicas = func() int {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return 1
	}
	return replicas, entered, release
}

// assertNotStalledByReplicas runs call in a goroutine, waits until it is
// inside Replicas, and checks that another limiter call still completes:
// the replica lookup (a cached Pod List that can block on informer sync)
// must not run under the limiter's lock.
func assertNotStalledByReplicas(t *testing.T, call func(rl *RateLimiter)) {
	t.Helper()
	replicas, entered, release := blockingReplicas()
	rl := NewRateLimiter(replicas)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); call(rl) }()
	defer wg.Wait()
	defer close(release)
	<-entered

	done := make(chan struct{})
	go func() { rl.AllowHeartbeat("team-b", "agent-1"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a limiter call stalled while another call waited on Replicas")
	}
}

func TestRateLimiter_AllowDoesNotHoldLockForReplicas(t *testing.T) {
	assertNotStalledByReplicas(t, func(rl *RateLimiter) {
		rl.Allow(rlTokenProvider(10, 1000), "team-a", "m1")
	})
}

func TestRateLimiter_DebitDoesNotHoldLockForReplicas(t *testing.T) {
	assertNotStalledByReplicas(t, func(rl *RateLimiter) {
		rl.DebitTokens(rlTokenProvider(0, 1000), "team-a", "m1", 10)
	})
}
