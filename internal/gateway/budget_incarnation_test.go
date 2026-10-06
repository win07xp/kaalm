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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// incarnation returns p's copy bound to uid: the same name, a new object.
func incarnation(p *kaalmv1beta1.ModelProvider, uid types.UID) *kaalmv1beta1.ModelProvider {
	out := p.DeepCopy()
	out.UID = uid
	return out
}

// A provider deleted and recreated under the same name starts from zero
// spend: the ledger built for the old UID is not the new provider's.
func TestBudgetLedger_RecreatedProviderStartsFromZero(t *testing.T) {
	base := budgetProvider(blockAt100())
	pA, pB := incarnation(base, "uid-a"), incarnation(base, "uid-b")
	b := NewBudgetLedger()
	b.Add(pA, "team-a", "agent/x", 150)
	if d := b.Enforce(pA, "team-a"); d.Action != kaalmv1beta1.BudgetActionBlock {
		t.Fatalf("old incarnation at 150%% = %+v, want block", d)
	}
	if d := b.Enforce(pB, "team-a"); d.Action != "" {
		t.Errorf("recreated provider inherited the old spend: %+v", d)
	}
}

// The first tick after a recreate runs publish before fold; the old
// incarnation's counters must never be published for the new provider.
func TestBudgetPublisher_PublishSkipsPreviousIncarnation(t *testing.T) {
	ctx := context.Background()
	base := budgetProvider(blockAt100())
	pA, pB := incarnation(base, "uid-a"), incarnation(base, "uid-b")
	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Providers: providersFn(pB)}

	ledger.Add(pA, "team-a", "agent/x", 42)
	pub.publish(ctx, pB)
	cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := cm.Data["gw-0"]; ok {
		t.Fatalf("published the old incarnation's counters for the new provider: %s", raw)
	}

	ledger.Add(pB, "team-a", "agent/x", 3)
	pub.publish(ctx, pB)
	cm, err = client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, spend, _, uid, err := ParseBudgetPartial(cm.Data["gw-0"])
	if err != nil || spend["team-a"] != 3 || uid != "uid-b" {
		t.Errorf("new incarnation's partial = %v uid=%q err=%v, want team-a 3 tagged uid-b", spend, uid, err)
	}
}

// Calls that still carry the deleted provider (an in-flight settle, a
// request that read the old object) neither write into nor rebind the new
// provider's ledger.
func TestBudgetLedger_SupersededIncarnationCannotWrite(t *testing.T) {
	base := budgetProvider(blockAt100())
	pA, pB := incarnation(base, "uid-a"), incarnation(base, "uid-b")
	b := NewBudgetLedger()
	b.Add(pA, "team-a", "agent/x", 70)
	b.Add(pB, "team-a", "agent/x", 10) // the recreated provider binds
	b.Enforce(pA, "team-a")            // a late read of the old object
	b.Add(pA, "team-a", "agent/x", 50)
	b.Enforce(pA, "team-a")
	b.mu.Lock()
	own := b.providers["prov"].own["team-a"]
	b.mu.Unlock()
	if own != 10 {
		t.Errorf("new incarnation's own spend = %v, want 10 (old writes dropped, no rebind)", own)
	}

	// Hard mode: a settle taken from the old incarnation must not free the
	// new incarnation's slot or add to its spend.
	hA := incarnation(hardProvider(blockAt100()), "uid-a")
	hB := incarnation(hardProvider(blockAt100()), "uid-b")
	h, _ := fakeClockLedger(hA)
	h.Add(hA, "team-a", "agent/x", 96)
	_, settleA := h.Admit(hA, "team-a", "agent/x")
	if settleA == nil {
		t.Fatal("expected a boundary admission for the old incarnation")
	}
	h.FoldPeers(hB, map[string]float64{"team-a": 96})
	h.Add(hB, "team-a", "agent/x", 0.5)
	_, settleB := h.Admit(hB, "team-a", "agent/x")
	if settleB == nil {
		t.Fatal("expected a boundary admission for the new incarnation")
	}
	settleA(5)
	if d, s := h.Admit(hB, "team-a", "agent/x"); !d.Throttled || s != nil {
		t.Errorf("old settle freed the new incarnation's slot: %+v", d)
	}
	h.mu.Lock()
	ownB := h.providers["prov"].own["team-a"]
	h.mu.Unlock()
	if ownB != 0.5 {
		t.Errorf("new incarnation's own spend = %v, want 0.5 (old settle dropped)", ownB)
	}
}

// Folds skip values tagged with another incarnation; untagged values count
// as the current provider's.
func TestFoldPartials_SkipsOtherIncarnation(t *testing.T) {
	period := PeriodKey("monthly", time.Now())
	data := map[string]string{
		"gw-1":     `{"period":"` + period + `","team-a":"50.00","_providerUID":"uid-a"}`,
		"gw-2":     `{"period":"` + period + `","team-a":"5.00"}`,
		RetiredKey: `{"period":"` + period + `","team-a":"7.00","_providerUID":"uid-a"}`,
	}
	if got := FoldPartials(data, "gw-0", period, "uid-b"); got["team-a"] != 5 {
		t.Errorf("fold = %v, want team-a 5 (only the untagged peer)", got)
	}
	if got := FoldPartials(data, "gw-0", period, "uid-a"); got["team-a"] != 62 {
		t.Errorf("fold for uid-a = %v, want team-a 62", got)
	}

	gotPeriod, spend, _, uid, err := ParseBudgetPartial(data["gw-1"])
	if err != nil || gotPeriod != period || uid != "uid-a" {
		t.Fatalf("parse = %q uid=%q err=%v", gotPeriod, uid, err)
	}
	if _, leaked := spend[providerUIDField]; leaked || len(spend) != 1 {
		t.Errorf("the provider tag leaked into spend: %v", spend)
	}
}

// The watch-driven fold skips a peer key written for the deleted provider.
func TestFoldBudgetConfigMapEvent_SkipsOtherIncarnation(t *testing.T) {
	store := newFakeStore()
	pB := incarnation(budgetProvider(blockAt100()), "uid-b")
	store.providers["prov"] = pB
	b := NewBudgetLedger()
	period := PeriodKey("monthly", time.Now())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kaalm-budget-prov", Namespace: "kaalm-system"},
		Data: map[string]string{
			"gw-1": `{"period":"` + period + `","team-a":"150.00","_providerUID":"uid-a"}`,
		},
	}
	FoldBudgetConfigMapEvent(context.Background(), cm, "gw-0", store, b)
	if d := b.Enforce(pB, "team-a"); d.Action != "" {
		t.Errorf("folded the old incarnation's peer key: %+v", d)
	}
}
