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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestBudgetConfigMapName(t *testing.T) {
	if got := BudgetConfigMapName("prov"); got != "kaalm-budget-prov" {
		t.Errorf("BudgetConfigMapName = %q", got)
	}
}

func TestBudgetLedger_InitCanonical(t *testing.T) {
	p := budgetProvider(kaalmv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()
	// Seed peer view from the reconciler's roll-up; own is still zero.
	b.InitCanonical(p, map[string]float64{"team-a": 110})
	if d := b.Enforce(p, "team-a"); d.Action != kaalmv1alpha1.BudgetActionBlock {
		t.Errorf("canonical seed of 110%% should block, got %+v", d)
	}
}

// providersFn returns a static provider set for the publisher.
func providersFn(ps ...*kaalmv1alpha1.ModelProvider) func(context.Context) []*kaalmv1alpha1.ModelProvider {
	return func(context.Context) []*kaalmv1alpha1.ModelProvider { return ps }
}

func TestBudgetPublisher_PublishAndFold(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})

	// The reconciler owns the ConfigMap; the fake clientset does not support
	// server-side-apply create, so seed the (empty) object the Apply patches.
	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", 42) // own spend to publish

	pub := &BudgetPublisher{
		Client:            client,
		Ledger:            ledger,
		OperatorNamespace: "kaalm-system",
		PodName:           "gw-0",
		Providers:         providersFn(p),
	}

	// publish server-side-applies this replica's partial under its Pod key.
	pub.publish(ctx, p)
	cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("published ConfigMap missing: %v", err)
	}
	raw, ok := cm.Data["gw-0"]
	if !ok {
		t.Fatalf("own partial key missing: %v", cm.Data)
	}
	period, spend, err := ParseBudgetPartial(raw)
	if err != nil || period != PeriodKey("monthly", time.Now()) || spend["team-a"] != 42 {
		t.Errorf("published partial wrong: %q %v %v", period, spend, err)
	}

	// A peer replica writes its own partial; fold must pick it up (excluding
	// our own key and _canonical).
	peerPartial, _ := json.Marshal(budgetPartial{
		Period: PeriodKey("monthly", time.Now()),
		Spend:  map[string]string{"team-a": "70.00"},
	})
	cm.Data["gw-1"] = string(peerPartial)
	cm.Data[CanonicalKey] = `{"period":"ignored","team-a":"999"}`
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pub.fold(ctx, p)
	// own 42 + peer 70 = 112% -> block (canonical key must be ignored).
	if d := ledger.Enforce(p, "team-a"); d.Action != kaalmv1alpha1.BudgetActionBlock {
		t.Errorf("fold should fold peer 70 in, got %+v", d)
	}
}

func TestBudgetPublisher_FoldDropsStalePeriod(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	client := k8sfake.NewSimpleClientset()
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}

	// A peer partial tagged with a stale period must be dropped by the fold.
	stalePartial, _ := json.Marshal(budgetPartial{Period: "1999-01", Spend: map[string]string{"team-a": "500"}})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
		Data:       map[string]string{"gw-9": string(stalePartial)},
	}
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pub.fold(ctx, p)
	if d := ledger.Enforce(p, "team-a"); d.Action != "" {
		t.Errorf("stale-period peer must be dropped, got %+v", d)
	}
}

func TestBudgetPublisher_FoldMissingConfigMap(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider()
	client := k8sfake.NewSimpleClientset()
	pub := &BudgetPublisher{Client: client, Ledger: NewBudgetLedger(), OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}
	// No ConfigMap exists: fold must be a quiet no-op (NotFound tolerated).
	pub.fold(ctx, p)
}

func TestBudgetPublisher_PublishNoOwnSpend(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider()
	client := k8sfake.NewSimpleClientset()
	// Ledger has no spend; publish must not write anything.
	pub := &BudgetPublisher{Client: client, Ledger: NewBudgetLedger(), OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}
	pub.publish(ctx, p)
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{}); err == nil {
		t.Error("publish with no own spend must not create a ConfigMap")
	}
}

func TestBudgetPublisher_SeedFromCanonical(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})

	canonical, _ := json.Marshal(map[string]string{"team-a": "130"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
		Data:       map[string]string{CanonicalKey: string(canonical)},
	}
	client := k8sfake.NewSimpleClientset(cm)
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}

	pub.SeedFromCanonical(ctx)
	if d := ledger.Enforce(p, "team-a"); d.Action != kaalmv1alpha1.BudgetActionBlock {
		t.Errorf("canonical seed of 130%% should block, got %+v", d)
	}

	// A provider with no ConfigMap at all is skipped without panic.
	other := budgetProvider()
	other.Name = "other"
	pub2 := &BudgetPublisher{Client: k8sfake.NewSimpleClientset(), Ledger: NewBudgetLedger(), OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(other)}
	pub2.SeedFromCanonical(ctx)
}

func TestBudgetPublisher_TickAndRun(t *testing.T) {
	p := budgetProvider(kaalmv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	// A provider with budget disabled (period none) must be skipped by tick.
	disabled := budgetProvider()
	disabled.Name = "disabled"
	disabled.Spec.Budget.Period = "none"

	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", 10)
	pub := &BudgetPublisher{
		Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Interval: 5 * time.Millisecond,
		Providers: providersFn(p, disabled),
	}

	// tick publishes+folds the active provider and skips the disabled one.
	pub.tick(context.Background())
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Get(context.Background(), BudgetConfigMapName("prov"), metav1.GetOptions{}); err != nil {
		t.Errorf("tick should have published prov: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Get(context.Background(), BudgetConfigMapName("disabled"), metav1.GetOptions{}); err == nil {
		t.Error("tick must skip a period=none provider")
	}

	// Run loops until the context is cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestBudgetPublisher_RunDefaultInterval(t *testing.T) {
	// Interval 0 falls back to the 10s default; cancel immediately so the
	// ticker never fires and Run returns via ctx.Done.
	pub := &BudgetPublisher{
		Client: k8sfake.NewSimpleClientset(), Ledger: NewBudgetLedger(),
		OperatorNamespace: "kaalm-system", PodName: "gw-0",
		Providers: providersFn(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with default interval did not return")
	}
}
