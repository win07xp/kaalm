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

// allowLLM is Allow with no token estimate and without the Retry-After, for
// tests that count admissions.
func allowLLM(rl *RateLimiter, p *kaalmv1beta1.ModelProvider, namespace, model string) bool {
	ok, _, _ := rl.Allow(p, namespace, model, tokenEstimate{})
	return ok
}

// admit is Allow on team-a/m1 that must admit; it returns the reservation.
func admit(t *testing.T, rl *RateLimiter, p *kaalmv1beta1.ModelProvider, est tokenEstimate) *tokenReservation {
	t.Helper()
	ok, retry, res := rl.Allow(p, "team-a", "m1", est)
	if !ok {
		t.Fatalf("Allow(%+v) refused with Retry-After %d, want admitted", est, retry)
	}
	return res
}

// expectRefused checks the next team-a/m1 request is refused with retry.
func expectRefused(t *testing.T, rl *RateLimiter, p *kaalmv1beta1.ModelProvider, retry int) {
	t.Helper()
	if ok, got, res := rl.Allow(p, "team-a", "m1", tokenEstimate{}); ok || got != retry || res != nil {
		t.Errorf("Allow = (%v, %d, %v), want refused with Retry-After %d and no reservation", ok, got, res, retry)
	}
}

func rlTokenProvider(rpm, tpm int32) *kaalmv1beta1.ModelProvider {
	p := rlProvider(rpm)
	p.Spec.RateLimits.TokensPerMinute = tpm
	return p
}

// TestRateLimiter_TokenDebitBlocksNextRequest: settled usage larger than the
// reservation drives the token bucket below zero, so the next request is
// refused with a Retry-After computed from the debt, and admitted again once
// the bucket refills above zero.
func TestRateLimiter_TokenDebitBlocksNextRequest(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{}).settle(1500) // 1000 - 1500 = -500
	ok, retry, _ := rl.Allow(p, "team-a", "m1", tokenEstimate{})
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
	expectRefused(t, rl, p, 1)
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

	admit(t, rl, p, tokenEstimate{}).settle(1000) // 0 left
	now = now.Add(30 * time.Millisecond)          // refills half a token
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

	admit(t, rl, p, tokenEstimate{}).settle(1_000_000)
	expectRefused(t, rl, p, 60) // the debt clamps at -1000, a minute at 1000/min
}

// TestRateLimiter_TokenDebtReclampedWhenReplicasGrow: a debt taken at a
// larger share is re-clamped to minus one burst at the current share once
// the replica count grows, so it still blocks for at most a minute.
func TestRateLimiter_TokenDebtReclampedWhenReplicasGrow(t *testing.T) {
	replicas := 1
	rl := NewRateLimiter(func() int { return replicas })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{}).settle(1_000_000) // clamps at -1000
	replicas = 2                                       // share 500
	expectRefused(t, rl, p, 60)                        // one burst at the new share
}

// TestRateLimiter_TokenDebtReclampedBeforeRefill: the re-clamp runs before
// the elapsed refill is credited, so when no call reaches the key before the
// replica count grows partway through, the key admits again one minute after
// the debit.
func TestRateLimiter_TokenDebtReclampedBeforeRefill(t *testing.T) {
	replicas := 1
	rl := NewRateLimiter(func() int { return replicas })
	t0 := time.Now()
	now := t0
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{}).settle(1_000_000) // -1000 at t0
	now = t0.Add(30 * time.Second)
	replicas = 2 // share 500: -500 + 250 = -250
	expectRefused(t, rl, p, 30)
	now = t0.Add(60 * time.Second)
	expectRefused(t, rl, p, 1)
	now = t0.Add(61 * time.Second)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("at t0+61s: want admitted")
	}
}

// TestRateLimiter_TokenDebtReclampedWhenCeilingDrops: lowering
// tokensPerMinute shrinks the share like a replica-count increase, and the
// debt is re-clamped the same way.
func TestRateLimiter_TokenDebtReclampedWhenCeilingDrops(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }

	admit(t, rl, rlTokenProvider(0, 1000), tokenEstimate{}).settle(1_000_000) // -1000
	expectRefused(t, rl, rlTokenProvider(0, 250), 60)
}

// TestRateLimiter_TokenDebtNotDeepenedWhenReplicasShrink: a larger share
// leaves the debt as it is; it only refills faster.
func TestRateLimiter_TokenDebtNotDeepenedWhenReplicasShrink(t *testing.T) {
	replicas := 2
	rl := NewRateLimiter(func() int { return replicas })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{}).settle(1_000_000) // clamps at -500
	replicas = 1                                       // share 1000
	expectRefused(t, rl, p, 30)
}

// TestRateLimiter_TokensDivideByReplicas: the token ceiling splits across
// live replicas like the request ceiling.
func TestRateLimiter_TokensDivideByReplicas(t *testing.T) {
	rl := NewRateLimiter(func() int { return 4 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 4000) // per replica 1000

	admit(t, rl, p, tokenEstimate{}).settle(1500)
	expectRefused(t, rl, p, 30)
}

// TestRateLimiter_NoTokenLimitIgnoresDebits: an unset tokensPerMinute keeps
// the request-only behavior: Allow reserves nothing and returns no
// reservation, whose methods do nothing.
func TestRateLimiter_NoTokenLimitIgnoresDebits(t *testing.T) {
	rl := NewRateLimiter(nil)
	p := rlTokenProvider(0, 0)
	res := admit(t, rl, p, tokenEstimate{Input: 1_000_000, Output: outputCapUnknown})
	if res != nil {
		t.Fatalf("reservation = %+v, want nil with no tokensPerMinute", res)
	}
	res.settle(1_000_000)
	res.keep()
	res.release()
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("with no tokensPerMinute a large call must not block")
	}
}

// TestRateLimiter_TokenRefusalKeepsRequestToken: a request the token bucket
// refuses does not spend a request token.
func TestRateLimiter_TokenRefusalKeepsRequestToken(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(2, 60) // 2 requests/min, 1 token/s

	admit(t, rl, p, tokenEstimate{}).settle(61) // -1, one request token left
	for i := 0; i < 3; i++ {
		if allowLLM(rl, p, "team-a", "m1") {
			t.Fatal("a bucket in debt must refuse")
		}
	}
	now = now.Add(2 * time.Second) // tokens back to +1; the request token is still there
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("the request token must survive the token refusals")
	}
}

// TestRateLimiter_ReservationComesOffAtAdmission: Allow takes the estimate
// off the token bucket, so concurrent calls admitted while it is positive
// cannot together overshoot the share by more than one reservation.
func TestRateLimiter_ReservationComesOffAtAdmission(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{Input: 100, Output: 400}) // 500
	admit(t, rl, p, tokenEstimate{Input: 100, Output: 400}) // 0
	expectRefused(t, rl, p, 1)
}

// TestRateLimiter_SettleReplacesReservationWithUsage: settle credits the
// reservation back and debits the usage, once.
func TestRateLimiter_SettleReplacesReservationWithUsage(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	res := admit(t, rl, p, tokenEstimate{Output: 500}) // 500
	res.settle(100)                                    // 900
	res.release()                                      // done: no effect
	res.settle(100)                                    // done: no effect
	admit(t, rl, p, tokenEstimate{Output: 1000})       // -100
	expectRefused(t, rl, p, 6)
}

// TestRateLimiter_ReleaseReturnsOnlyWhatWasTaken: a reservation the floor
// cut short credits back only what came off, so a release cannot lift the
// bucket above where it stood before the reservation.
func TestRateLimiter_ReleaseReturnsOnlyWhatWasTaken(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{Output: 700})         // 300
	big := admit(t, rl, p, tokenEstimate{Output: 5000}) // -1000 (the floor), 1300 taken
	big.release()                                       // 300
	admit(t, rl, p, tokenEstimate{Output: 300})         // 0
	expectRefused(t, rl, p, 1)
}

// TestRateLimiter_KeepHoldsReservation: keep leaves the reservation as the
// call's charge, and a later release does nothing.
func TestRateLimiter_KeepHoldsReservation(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	res := admit(t, rl, p, tokenEstimate{Output: 1500}) // -500
	res.keep()
	expectRefused(t, rl, p, 30)
	res.release()
	expectRefused(t, rl, p, 30)
}

// TestRateLimiter_SettleAtLeastChargesTheLarger: settleAtLeast, for a call
// that did not finish cleanly, keeps the reservation when the usage read is
// smaller and settles the usage when it is larger, once.
func TestRateLimiter_SettleAtLeastChargesTheLarger(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	small := admit(t, rl, p, tokenEstimate{Output: 500}) // 500
	small.settleAtLeast(7)                               // keeps 500 taken: 500
	small.settle(7)                                      // done: no effect
	big := admit(t, rl, p, tokenEstimate{Output: 100})   // 400
	big.settleAtLeast(300)                               // 400 + 100 - 300 = 200
	admit(t, rl, p, tokenEstimate{Output: 200})          // 0
	expectRefused(t, rl, p, 1)
}

// TestRateLimiter_NoOutputCapReservesOneShare: with no output cap known,
// the reservation is the input estimate plus one replica share.
func TestRateLimiter_NoOutputCapReservesOneShare(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	admit(t, rl, p, tokenEstimate{Input: 10, Output: outputCapUnknown}) // -10
	expectRefused(t, rl, p, 1)
}

// TestRateLimiter_SettleAfterRefillClampsAtBurst: the credit is clamped at
// the burst before the usage comes off, so a bucket that refilled during a
// long call ends where a debit at the end would leave it.
func TestRateLimiter_SettleAfterRefillClampsAtBurst(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	res := admit(t, rl, p, tokenEstimate{Output: 1000}) // 0
	now = now.Add(time.Minute)                          // refilled to 1000
	res.settle(500)                                     // 1000, not 2000, minus 500
	admit(t, rl, p, tokenEstimate{Output: 600})         // -100
	expectRefused(t, rl, p, 6)
}

// TestRateLimiter_SettleWhenShareShrankMidCall: settle reads the current
// share, so the credit cannot lift the bucket past the new, smaller burst.
func TestRateLimiter_SettleWhenShareShrankMidCall(t *testing.T) {
	replicas := 1
	rl := NewRateLimiter(func() int { return replicas })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(0, 1000)

	res := admit(t, rl, p, tokenEstimate{Output: 1000}) // 0
	replicas = 2                                        // share 500
	res.settle(100)                                     // min(1000, 500) - 100 = 400
	admit(t, rl, p, tokenEstimate{Output: 500})         // -100
	expectRefused(t, rl, p, 12)                         // 100 at 500/min
}

// TestRateLimiter_RequestRefusalReservesNothing: a request the request
// bucket refuses takes nothing off the token bucket.
func TestRateLimiter_RequestRefusalReservesNothing(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlTokenProvider(1, 1000)

	admit(t, rl, p, tokenEstimate{Output: 100}) // 900
	if ok, _, res := rl.Allow(p, "team-a", "m1", tokenEstimate{Output: 100}); ok || res != nil {
		t.Fatalf("Allow = (%v, %v), want refused by the request bucket with no reservation", ok, res)
	}
	if got := rl.buckets[tokenKey("team-a", "m1")].tokens; got != 900 {
		t.Errorf("token bucket = %v, want 900", got)
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
	ok, retry, _ := rl.Allow(p, "team-a", "m1", tokenEstimate{})
	if ok || retry != 30 {
		t.Errorf("drained 2/min bucket: ok=%v retry=%d, want refused with 30", ok, retry)
	}
	now = now.Add(15 * time.Second)
	if _, retry, _ = rl.Allow(p, "team-a", "m1", tokenEstimate{}); retry != 15 {
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
		if ok, wait := rl.AllowTool(unlimited, "team-a"); !ok || wait != 0 {
			t.Fatalf("a ToolProvider with no rate limit must always allow, got (%v, %d)", ok, wait)
		}
	}

	tp := &kaalmv1beta1.ToolProvider{}
	tp.Name = "search"
	tp.Spec.RateLimits.RequestsPerMinute = 2
	allowed := 0
	for i := 0; i < 5; i++ {
		ok, wait := rl.AllowTool(tp, "team-a")
		switch {
		case ok && wait != 0:
			t.Errorf("an admitted call returned wait %d, want 0", wait)
		case ok:
			allowed++
		case wait < 1:
			t.Errorf("a refused call returned wait %d, want at least 1", wait)
		}
	}
	if allowed != 2 {
		t.Errorf("allowed %d calls, want the 2-per-minute ceiling", allowed)
	}
	// A different namespace holds its own bucket. (Cross-plane collisions
	// are unreachable by construction: the "mcp:" prefix contains a
	// character no namespace name can, so no LLM key can equal a tool key.)
	if ok, _ := rl.AllowTool(tp, "team-b"); !ok {
		t.Error("second namespace must carry its own bucket")
	}
}

// TestAllowTool_RetryAfterFromBucket: a refused call reports the seconds
// until the tool bucket holds a call again, computed as for the LLM request
// bucket.
func TestAllowTool_RetryAfterFromBucket(t *testing.T) {
	rl := NewRateLimiter(nil)
	now := time.Now()
	rl.now = func() time.Time { return now }
	tp := &kaalmv1beta1.ToolProvider{}
	tp.Name = "search"
	tp.Spec.RateLimits.RequestsPerMinute = 2 // one call every 30s

	for i := 0; i < 2; i++ {
		if ok, _ := rl.AllowTool(tp, "team-a"); !ok {
			t.Fatalf("call %d must fit the burst of 2", i+1)
		}
	}
	if ok, wait := rl.AllowTool(tp, "team-a"); ok || wait != 30 {
		t.Errorf("third call = (%v, %d), want (false, 30)", ok, wait)
	}
}

// TestAllowTool_FewerCallsThanReplicas: with requestsPerMinute below the
// replica count, the tool bucket still holds one whole call, so the key is
// admitted once per refill instead of refused forever, and a refusal
// reports the wait until the next call fits.
func TestAllowTool_FewerCallsThanReplicas(t *testing.T) {
	rl := NewRateLimiter(func() int { return 3 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	tp := &kaalmv1beta1.ToolProvider{}
	tp.Name = "search"
	tp.Spec.RateLimits.RequestsPerMinute = 2 // 2/3 call per minute per replica

	if ok, _ := rl.AllowTool(tp, "team-a"); !ok {
		t.Fatal("a fresh bucket must admit one call even when the share is below 1")
	}
	if ok, wait := rl.AllowTool(tp, "team-a"); ok || wait != 90 {
		t.Fatalf("drained bucket = (%v, %d), want (false, 90)", ok, wait)
	}
	now = now.Add(60 * time.Second)
	// About 0.67 of a call has refilled, so a third of a call (30s) is
	// missing. The exact 30s wait is not rounded up to 31.
	if ok, wait := rl.AllowTool(tp, "team-a"); ok || wait != 30 {
		t.Errorf("after 60s = (%v, %d), want (false, 30)", ok, wait)
	}
	now = now.Add(30 * time.Second)
	if ok, wait := rl.AllowTool(tp, "team-a"); !ok || wait != 0 {
		t.Errorf("a bucket refilled to one call (90s at 2/3 per minute) = (%v, %d), want (true, 0)", ok, wait)
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
// per refill instead of refused forever. Sixty seconds after the
// drain, a third of a request (30s) is missing, and the refusal reports
// exactly 30.
func TestRateLimiter_FewerRequestsThanReplicas(t *testing.T) {
	rl := NewRateLimiter(func() int { return 3 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(2) // 2/3 request per minute per replica

	if !allowLLM(rl, p, "team-a", "m1") {
		t.Fatal("a fresh bucket must admit one request even when the share is below 1")
	}
	ok, retry, _ := rl.Allow(p, "team-a", "m1", tokenEstimate{})
	if ok || retry != 90 { // 1 request at 2/3 per minute is 90s
		t.Errorf("drained bucket: ok=%v retry=%d, want refused with 90", ok, retry)
	}
	now = now.Add(60 * time.Second)
	if ok, retry, _ := rl.Allow(p, "team-a", "m1", tokenEstimate{}); ok || retry != 30 {
		t.Errorf("after 60s: ok=%v retry=%d, want refused with 30", ok, retry)
	}
	now = now.Add(30 * time.Second)
	if !allowLLM(rl, p, "team-a", "m1") {
		t.Error("a bucket refilled to one request must admit")
	}
}

// TestRetryAfterSeconds: the Retry-After is the ceiling of the exact wait,
// so float error in a whole-second wait does not add a second, a
// real fraction still rounds up, and the result is at least 1.
func TestRetryAfterSeconds(t *testing.T) {
	// Built from runtime values, as production does: a constant expression
	// such as 1 - 2.0/3 is folded exactly and would hide the float error.
	share := perReplica(2, 3)
	refilled := 1.0 * share // one minute of refill
	cases := []struct {
		name               string
		deficit, perMinute float64
		want               int
	}{
		{"third of a call at 2/3 per minute", 1 - refilled, share, 30},
		{"half a call at 1 per minute", 0.5, 1, 30},
		{"token debt of half a minute", 500, 1000, 30},
		{"one call at 2/3 per minute", 1, share, 90},
		{"real fraction rounds up", 0.5000001, 1, 31},
		{"zero deficit floors at 1", 0, 2, 1},
		{"sub-second wait floors at 1", 1e-6, 60, 1},
	}
	for _, tc := range cases {
		if got := retryAfterSeconds(tc.deficit, tc.perMinute); got != tc.want {
			t.Errorf("%s: retryAfterSeconds(%v, %v) = %d, want %d", tc.name, tc.deficit, tc.perMinute, got, tc.want)
		}
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
		rl.Allow(rlTokenProvider(10, 1000), "team-a", "m1", tokenEstimate{Output: 10})
	})
}

func TestRateLimiter_SettleDoesNotHoldLockForReplicas(t *testing.T) {
	assertNotStalledByReplicas(t, func(rl *RateLimiter) {
		res := &tokenReservation{r: rl, limit: 1000, namespace: "team-a", model: "m1", taken: 10}
		res.settle(10)
	})
}

func TestRateLimiter_ReleaseDoesNotHoldLockForReplicas(t *testing.T) {
	assertNotStalledByReplicas(t, func(rl *RateLimiter) {
		res := &tokenReservation{r: rl, limit: 1000, namespace: "team-a", model: "m1", taken: 10}
		res.release()
	})
}
