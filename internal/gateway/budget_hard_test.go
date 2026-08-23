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
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// hardProvider is budgetProvider flipped to hard enforcement with the
// default 5 point margin.
func hardProvider(policies ...kaalmv1beta1.ModelProviderBudgetPolicy) *kaalmv1beta1.ModelProvider {
	mp := budgetProvider(policies...)
	mp.Spec.Budget.Enforcement = kaalmv1beta1.BudgetEnforcementHard
	mp.Spec.Budget.Hard = &kaalmv1beta1.ModelProviderBudgetHard{BoundaryMarginPercent: 5}
	return mp
}

func blockAt100() kaalmv1beta1.ModelProviderBudgetPolicy {
	return kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: kaalmv1beta1.BudgetActionBlock}
}

// fakeClockLedger builds a ledger on an atomically advanceable clock, with a
// fresh fold so the read-path staleness signal starts healthy.
func fakeClockLedger(mp *kaalmv1beta1.ModelProvider) (*BudgetLedger, *int64) {
	start := time.Date(2099, 6, 15, 12, 0, 0, 0, time.UTC)
	nanos := start.UnixNano()
	b := NewBudgetLedger()
	b.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nanos)) }
	b.FoldPeers(mp, map[string]float64{})
	return b, &nanos
}

func advance(nanos *int64, d time.Duration) { atomic.AddInt64(nanos, int64(d)) }

// Test 1: serialized admission. In the boundary region the second concurrent
// request throttles, and settle-and-free is atomic: the next admit sees the
// settled cost, crossing into block when it should.
func TestHardAdmit_SerializedAdmission(t *testing.T) {
	mp := hardProvider(blockAt100())
	b, _ := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/test", 96) // boundary starts at 95

	d1, settle1 := b.Admit(mp, "team-a", "agent/test")
	if settle1 == nil || !d1.BoundaryEngaged || d1.Throttled {
		t.Fatalf("first admit = %+v settle=%v, want engaged with settle", d1, settle1 != nil)
	}
	d2, settle2 := b.Admit(mp, "team-a", "agent/test")
	if !d2.Throttled || settle2 != nil {
		t.Fatalf("second admit = %+v, want throttled", d2)
	}
	settle1(5) // 96 + 5 = 101%
	d3, settle3 := b.Admit(mp, "team-a", "agent/test")
	if d3.Action != kaalmv1beta1.BudgetActionBlock || settle3 != nil {
		t.Fatalf("post-settle admit = %+v, want block (settled cost must be visible atomically)", d3)
	}
	if d3.Ceiling != "namespace" {
		t.Fatalf("ceiling attribution = %q, want namespace", d3.Ceiling)
	}
}

// Test 2: the sticky slot. A fold that drops utilization below the boundary
// (a peer prune during a rollout) must not unblock a held slot.
func TestHardAdmit_StickySlot(t *testing.T) {
	mp := hardProvider(blockAt100())
	b, _ := fakeClockLedger(mp)
	b.FoldPeers(mp, map[string]float64{"team-a": 96})

	_, settle := b.Admit(mp, "team-a", "agent/test")
	if settle == nil {
		t.Fatal("expected boundary admission")
	}
	b.FoldPeers(mp, map[string]float64{}) // peers pruned: utilization drops to 0
	if d, s := b.Admit(mp, "team-a", "agent/test"); !d.Throttled || s != nil {
		t.Fatalf("admit with held slot after fold = %+v, want throttled", d)
	}
	settle(0)
	if d, s := b.Admit(mp, "team-a", "agent/test"); d.Throttled || s != nil || d.BoundaryEngaged {
		t.Fatalf("admit after settle at 0%% = %+v settle=%v, want plain pass", d, s != nil)
	}
}

// Test 3: settle idempotence and rollover. Double-settle lands the cost
// once; a settle crossing midnight lands in the new period without touching
// the cleared slot map; the post-rollover admit is slot-free.
func TestHardAdmit_SettleIdempotenceAndRollover(t *testing.T) {
	mp := hardProvider(blockAt100())
	mp.Spec.Budget.Period = "daily"
	b, nanos := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/test", 96)

	_, settle := b.Admit(mp, "team-a", "agent/test")
	if settle == nil {
		t.Fatal("expected boundary admission")
	}
	settle(2)
	settle(2) // idempotent
	b.mu.Lock()
	got := b.providers["prov"].own["team-a"]
	b.mu.Unlock()
	if got != 98 {
		t.Fatalf("own after double settle = %v, want 98", got)
	}

	_, settle2 := b.Admit(mp, "team-a", "agent/test")
	if settle2 == nil {
		t.Fatal("expected boundary admission before midnight")
	}
	advance(nanos, 24*time.Hour) // period rollover
	settle2(3)                   // lands in the NEW period
	b.mu.Lock()
	l := b.providers["prov"]
	newOwn, slots := l.own["team-a"], len(l.adm)
	b.mu.Unlock()
	if newOwn != 3 || slots != 0 {
		t.Fatalf("post-rollover own=%v slots=%d, want 3 and 0", newOwn, slots)
	}
	b.FoldPeers(mp, map[string]float64{}) // refresh read path in the new period
	if d, s := b.Admit(mp, "team-a", "agent/test"); d.BoundaryEngaged || s != nil {
		t.Fatalf("post-rollover admit = %+v, want slot-free at ~3%%", d)
	}
}

// Test 4: the cluster ceiling gets a provider-wide slot serializing across
// namespaces; namespace ceilings keep independent slots.
func TestHardAdmit_ClusterCeilingSlot(t *testing.T) {
	mp := hardProvider(blockAt100())
	mp.Spec.Budget.PerNamespaceUSD = "1000" // ns ratios stay far from boundary
	cluster := "100"
	mp.Spec.Budget.ClusterUSD = &cluster
	b, _ := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/test", 50)
	b.Add(mp, "team-b", "agent/test", 46) // cluster at 96%

	d1, settle1 := b.Admit(mp, "team-a", "agent/test")
	if settle1 == nil || !d1.BoundaryEngaged {
		t.Fatalf("team-a admit = %+v, want cluster-boundary admission", d1)
	}
	if d2, s2 := b.Admit(mp, "team-b", "agent/test"); !d2.Throttled || s2 != nil {
		t.Fatalf("team-b admit with cluster slot held = %+v, want throttled", d2)
	}
	settle1(0)

	// Namespace-only boundaries stay independent across namespaces.
	mp2 := hardProvider(blockAt100())
	mp2.ObjectMeta = metav1.ObjectMeta{Name: "prov2"}
	b2, _ := fakeClockLedger(mp2)
	b2.Add(mp2, "team-a", "agent/test", 96)
	b2.Add(mp2, "team-b", "agent/test", 96)
	_, sA := b2.Admit(mp2, "team-a", "agent/test")
	dB, sB := b2.Admit(mp2, "team-b", "agent/test")
	if sA == nil || sB == nil || dB.Throttled {
		t.Fatalf("independent ns slots: a=%v b=%v dB=%+v", sA != nil, sB != nil, dB)
	}
	sA(0)
	sB(0)
}

// Test 5: fail-closed on either staleness signal, and MarkPublished must not
// clear a dirtySince newer than its snapshot.
func TestHardAdmit_FailClosed(t *testing.T) {
	mp := hardProvider(blockAt100())

	// Write path: an unpublishable settle older than the window.
	b, nanos := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/test", 96) // sets dirtySince
	b.FoldPeers(mp, nil)                  // keep the read path fresh at t0
	advance(nanos, 31*time.Second)
	b.FoldPeers(mp, nil) // read path fresh again; write path still stale
	if d, s := b.Admit(mp, "team-a", "agent/test"); !d.Unavailable || s != nil {
		t.Fatalf("stale-dirty admit = %+v, want fail-closed", d)
	}
	b.MarkPublished("prov", b.now())
	if d, s := b.Admit(mp, "team-a", "agent/test"); d.Unavailable || s == nil {
		t.Fatalf("post-publish admit = %+v, want admission", d)
	} else {
		s(0)
	}

	// Read path: no successful fold within the window.
	b2, nanos2 := fakeClockLedger(mp)
	b2.Add(mp2ForProv(mp), "team-a", "agent/test", 96)
	b2.MarkPublished("prov", b2.now()) // write path clean
	advance(nanos2, 31*time.Second)
	if d, s := b2.Admit(mp, "team-a", "agent/test"); !d.Unavailable || s != nil {
		t.Fatalf("stale-fold admit = %+v, want fail-closed", d)
	}
	b2.FoldPeers(mp, nil)
	if d, s := b2.Admit(mp, "team-a", "agent/test"); d.Unavailable || s == nil {
		t.Fatalf("post-fold admit = %+v, want admission", d)
	} else {
		s(0)
	}

	// MarkPublished with a snapshot older than the dirt does not clean it.
	b3, nanos3 := fakeClockLedger(mp)
	snapshot := b3.now()
	advance(nanos3, 5*time.Second)
	b3.Add(mp, "team-a", "agent/test", 96) // dirty at t+5
	b3.MarkPublished("prov", snapshot)
	b3.mu.Lock()
	stillDirty := !b3.providers["prov"].dirtySince.IsZero()
	b3.mu.Unlock()
	if !stillDirty {
		t.Fatal("MarkPublished cleared a dirtySince newer than its snapshot")
	}
}

// mp2ForProv exists to keep the fail-closed test readable: the same provider
// object is safe to share across ledgers.
func mp2ForProv(mp *kaalmv1beta1.ModelProvider) *kaalmv1beta1.ModelProvider { return mp }

// Test 6: the effective-margin formula and its wire flag. Observed traffic
// widens the boundary beyond the configured floor; the flag travels as a
// non-numeric underscore field that folds must never count as spend.
func TestHardAdmit_EffectiveMarginAndWireFlag(t *testing.T) {
	mp := hardProvider(blockAt100())
	b, nanos := fakeClockLedger(mp)
	b.replicas = func() int { return 3 }

	// Build the tracker: maxCostPerCall = 2; bucket 0 sums 2 over 10s, so
	// closing it sets peakRatePerSec = 0.2.
	b.Add(mp, "team-a", "agent/test", 2)
	advance(nanos, 10*time.Second)
	b.Add(mp, "team-a", "agent/test", 1)
	b.FoldPeers(mp, map[string]float64{"team-a": 80}) // total 83%

	// marginUSD = 3x2 + 2x0.2x30 = 18 of the 100 ceiling: boundary at 82.
	d, settle := b.Admit(mp, "team-a", "agent/test")
	if settle == nil || !d.BoundaryEngaged {
		t.Fatalf("admit at 83%% with widened margin = %+v, want engaged", d)
	}
	if !d.MarginRaisedNow {
		t.Fatal("expected the margin-raised rising edge")
	}
	settle(0)
	if d2, s2 := b.Admit(mp, "team-a", "agent/test"); d2.MarginRaisedNow {
		t.Fatal("margin-raised must fire only on the rising edge")
	} else if s2 != nil {
		s2(0)
	}

	period, spend, marginRaised, ok := b.OwnPartial("prov")
	if !ok || !marginRaised {
		t.Fatalf("OwnPartial marginRaised=%v ok=%v, want true", marginRaised, ok)
	}
	raw, err := json.Marshal(budgetPartial{Period: period, Spend: spend, MarginExceeded: marginRaised})
	if err != nil {
		t.Fatal(err)
	}
	gotPeriod, gotSpend, gotFlag, err := ParseBudgetPartial(string(raw))
	if err != nil || !gotFlag || gotPeriod != period {
		t.Fatalf("round trip: period=%q flag=%v err=%v", gotPeriod, gotFlag, err)
	}
	if _, exists := gotSpend[marginExceededField]; exists {
		t.Fatal("the flag leaked into the spend map")
	}
	folded := FoldPartials(map[string]string{"gw-1": string(raw)}, "gw-0", period)
	if folded[marginExceededField] != 0 {
		t.Fatal("fold counted the margin flag as namespace spend")
	}
}

// Test 7: the race hammer. Admit/settle against folds, publishes, and
// snapshots under -race; the slot invariant holds and no settle is lost.
func TestHardAdmit_RaceHammer(t *testing.T) {
	mp := hardProvider(blockAt100())
	b, _ := fakeClockLedger(mp)
	b.FoldPeers(mp, map[string]float64{"team-a": 96})

	var settled int64 // millicents, to stay integral
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, settle := b.Admit(mp, "team-a", "agent/test")
				if settle != nil {
					settle(0.001)
					atomic.AddInt64(&settled, 1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			b.FoldPeers(mp, map[string]float64{"team-a": 96})
			b.OwnPartial("prov")
			b.MarkPublished("prov", b.now())
		}
	}()
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.providers["prov"]
	if len(l.adm) != 0 {
		t.Fatalf("slots leaked: %v", l.adm)
	}
	want := float64(atomic.LoadInt64(&settled)) * 0.001
	if diff := l.own["team-a"] - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("own=%v want=%v: settles lost or duplicated", l.own["team-a"], want)
	}
}

// Test 10: the watch-driven fold. Own key, _canonical, and flag fields are
// excluded; peers and _retired are summed; soft providers fold too.
func TestFoldBudgetConfigMapEvent(t *testing.T) {
	store := newFakeStore()
	soft := budgetProvider(blockAt100())
	store.providers["prov"] = soft
	b := NewBudgetLedger()
	period := PeriodKey("monthly", time.Now())

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kaalm-budget-prov", Namespace: "kaalm-system"},
		Data: map[string]string{
			"gw-0":       `{"period":"` + period + `","team-a":"10.00"}`,
			"gw-1":       `{"period":"` + period + `","team-a":"20.00","_marginExceeded":"true"}`,
			RetiredKey:   `{"period":"` + period + `","team-a":"5.00"}`,
			CanonicalKey: `{"team-a":"999.00"}`,
		},
	}
	FoldBudgetConfigMapEvent(context.Background(), cm, "gw-0", store, b)

	b.mu.Lock()
	l := b.providers["prov"]
	peers, foldOK := l.peers["team-a"], !l.lastFoldOK.IsZero()
	b.mu.Unlock()
	if peers != 25 || !foldOK {
		t.Fatalf("folded peers=%v foldOK=%v, want 25 (gw-1 + _retired, never own or _canonical) and true", peers, foldOK)
	}

	// Non-budget ConfigMaps and unknown providers are ignored quietly.
	FoldBudgetConfigMapEvent(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}}, "gw-0", store, b)
	FoldBudgetConfigMapEvent(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kaalm-budget-ghost"}}, "gw-0", store, b)
}

// Test 11: the settle-publish kick. A boundary settle publishes immediately
// through the kick channel; a failed publish leaves the ledger dirty and the
// next tick retries and clears it.
func TestBudgetPublisher_SettleKick(t *testing.T) {
	mp := hardProvider(blockAt100())
	ledger := NewBudgetLedger()
	ledger.FoldPeers(mp, nil)
	ledger.Add(mp, "team-a", "agent/test", 96)

	// The fake clientset cannot SSA-create, so the ConfigMap must pre-exist
	// (same caveat as TestBudgetPublisher_PublishAndFold).
	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	pub := &BudgetPublisher{
		Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Interval: time.Hour, Providers: providersFn(mp),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Run(ctx)

	_, settle := ledger.Admit(mp, "team-a", "agent/test")
	if settle == nil {
		t.Fatal("expected boundary admission")
	}
	settle(1)

	deadline := time.Now().Add(2 * time.Second)
	for {
		cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
		if err == nil {
			if raw, ok := cm.Data["gw-0"]; ok {
				_, spend, _, perr := ParseBudgetPartial(raw)
				if perr == nil && spend["team-a"] == 97 {
					break // settle-published without waiting for the 1h tick
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("settle-publish never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ledger.mu.Lock()
	clean := ledger.providers["prov"].dirtySince.IsZero()
	ledger.mu.Unlock()
	if !clean {
		t.Fatal("dirtySince not cleared by the settle-publish")
	}
}

// Test 11b: a failed settle-publish keeps the write path dirty; the tick
// retry clears it once the ConfigMap exists.
func TestBudgetPublisher_SettleKickFailureThenTick(t *testing.T) {
	mp := hardProvider(blockAt100())
	ledger := NewBudgetLedger()
	client := k8sfake.NewSimpleClientset() // SSA against a missing CM fails
	pub := &BudgetPublisher{
		Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Interval: time.Hour, Providers: providersFn(mp),
	}
	ledger.Add(mp, "team-a", "agent/test", 1)
	pub.publishByName(context.Background(), "prov")
	ledger.mu.Lock()
	dirty := !ledger.providers["prov"].dirtySince.IsZero()
	ledger.mu.Unlock()
	if !dirty {
		t.Fatal("failed publish must leave the ledger dirty")
	}

	_, err := client.CoreV1().ConfigMaps("kaalm-system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pub.tick(context.Background())
	ledger.mu.Lock()
	clean := ledger.providers["prov"].dirtySince.IsZero()
	ledger.mu.Unlock()
	if !clean {
		t.Fatal("tick retry did not clear the write path")
	}
}

// Test 9 (walk half): a throttled hard candidate consumes a depth slot, is
// never attempted, and the walk proceeds to its children.
func TestFallback_ThrottledHardCandidateSkipsToChild(t *testing.T) {
	h := newFallbackHarness()
	child := h.provider("child", "openai")
	blocked := h.provider("blocked-hard", "openai", "child")
	primary := h.provider("primary", "openai", "blocked-hard")

	blocked.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Enforcement: kaalmv1beta1.BudgetEnforcementHard,
		Hard:        &kaalmv1beta1.ModelProviderBudgetHard{BoundaryMarginPercent: 5},
		Policies:    []kaalmv1beta1.ModelProviderBudgetPolicy{blockAt100()},
	}
	h.server.Budget.FoldPeers(blocked, map[string]float64{"team-a": 96})
	if _, settle := h.server.Budget.Admit(blocked, "team-a", "agent/test"); settle == nil {
		t.Fatal("setup: expected to hold blocked-hard's slot")
	}

	h.results["primary"] = 500 // fallbackable
	h.results["child"] = 200
	// blocked-hard has no scripted result: attempting it would register a
	// connect error and fail the assertions below.

	chosen, attempts, ok := h.run(primary)
	if !ok || chosen != "child" || attempts != 3 {
		t.Fatalf("throttled mid-walk: chosen=%q attempts=%d ok=%v, want child/3/true", chosen, attempts, ok)
	}
	_ = child
}
