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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestBudgetConfigMapName(t *testing.T) {
	if got := BudgetConfigMapName("prov"); got != "kaalm-budget-prov" {
		t.Errorf("BudgetConfigMapName = %q", got)
	}
}

func TestBudgetLedger_InitCanonical(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()
	// Seed peer view from the reconciler's roll-up; own is still zero.
	b.InitCanonical(p, map[string]float64{"team-a": 110})
	if d := b.Enforce(p, "team-a"); d.Action != kaalmv1beta1.BudgetActionBlock {
		t.Errorf("canonical seed of 110%% should block, got %+v", d)
	}
}

// providersFn returns a static provider set for the publisher.
func providersFn(ps ...*kaalmv1beta1.ModelProvider) func(context.Context) []*kaalmv1beta1.ModelProvider {
	return func(context.Context) []*kaalmv1beta1.ModelProvider { return ps }
}

func TestBudgetPublisher_PublishAndFold(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})

	// The reconciler owns the ConfigMap; the fake clientset does not support
	// server-side-apply create, so seed the (empty) object the Apply patches.
	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", "agent/test", 42) // own spend to publish

	pub := &BudgetPublisher{
		Client:            client,
		Ledger:            ledger,
		OperatorNamespace: "kaalm-system",
		PodName:           "gw-0",
		Providers:         providersFn(p),
	}

	// publish server-side-applies this replica's partial under its Pod key.
	pub.publish(ctx, p, nil)
	cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("published ConfigMap missing: %v", err)
	}
	raw, ok := cm.Data["gw-0"]
	if !ok {
		t.Fatalf("own partial key missing: %v", cm.Data)
	}
	period, spend, _, _, err := ParseBudgetPartial(raw)
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
	pub.fold(ctx, p, nil)
	// own 42 + peer 70 = 112% -> block (canonical key must be ignored).
	if d := ledger.Enforce(p, "team-a"); d.Action != kaalmv1beta1.BudgetActionBlock {
		t.Errorf("fold should fold peer 70 in, got %+v", d)
	}
}

func TestBudgetPublisher_FoldDropsStalePeriod(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
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
	pub.fold(ctx, p, nil)
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
	pub.fold(ctx, p, nil)
}

func TestBudgetPublisher_PublishNoOwnSpend(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider()
	client := k8sfake.NewSimpleClientset()
	// Ledger has no spend; publish must not write anything.
	pub := &BudgetPublisher{Client: client, Ledger: NewBudgetLedger(), OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}
	pub.publish(ctx, p, nil)
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{}); err == nil {
		t.Error("publish with no own spend must not create a ConfigMap")
	}
}

func TestBudgetPublisher_SeedFromCanonical(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})

	canonical, _ := json.Marshal(map[string]string{"team-a": "130"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
		Data:       map[string]string{CanonicalKey: string(canonical)},
	}
	client := k8sfake.NewSimpleClientset(cm)
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}

	pub.SeedFromCanonical(ctx)
	if d := ledger.Enforce(p, "team-a"); d.Action != kaalmv1beta1.BudgetActionBlock {
		t.Errorf("canonical seed of 130%% should block, got %+v", d)
	}

	// A provider with no ConfigMap at all is skipped without panic.
	other := budgetProvider()
	other.Name = "other"
	pub2 := &BudgetPublisher{Client: k8sfake.NewSimpleClientset(), Ledger: NewBudgetLedger(), OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(other)}
	pub2.SeedFromCanonical(ctx)
}

func TestBudgetPublisher_TickAndRun(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	// A provider with budget disabled (period none) must be skipped by tick.
	disabled := budgetProvider()
	disabled.Name = "disabled"
	disabled.Spec.Budget.Period = "none"

	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", "agent/test", 10)
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

// Spend settled after the last periodic publish reaches this replica's key
// when Run stops, so the reconciler's _retired fold keeps it once the
// replica is gone.
func TestBudgetPublisher_RunPublishesOnShutdown(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	disabled := budgetProvider()
	disabled.Name = "disabled"
	disabled.Spec.Budget.Period = "none"
	client := k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"},
	})
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{
		Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Interval: time.Hour,
		Providers: providersFn(p, disabled),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()

	// Settled after the last tick, outside the boundary region: no kick.
	ledger.Add(p, "team-a", "agent/test", 7)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(context.Background(), BudgetConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, spend, _, _, err := ParseBudgetPartial(cm.Data["gw-0"])
	if err != nil || spend["team-a"] != 7 {
		t.Errorf("own partial after shutdown = %q (%v), want team-a 7", cm.Data["gw-0"], err)
	}
	if _, err := client.CoreV1().ConfigMaps("kaalm-system").Get(context.Background(), BudgetConfigMapName("disabled"), metav1.GetOptions{}); err == nil {
		t.Error("the final publish must skip a period=none provider")
	}
}

// The reducer's previous-period archive is never folded as spend, whatever
// shape its value has.
func TestFoldPartials_SkipsPreviousArchive(t *testing.T) {
	period := PeriodKey("monthly", time.Now())
	data := map[string]string{
		"gw-1":      `{"period":"` + period + `","team-a":"5.00"}`,
		"_previous": `{"period":"` + period + `","team-a":"40.00"}`,
	}
	if got := FoldPartials(data, "gw-0", period, ""); got["team-a"] != 5 {
		t.Errorf("fold = %v, want team-a 5 (the _previous archive is not spend)", got)
	}
}

// Every publish labels the ConfigMaps it writes, so each replica's tick can
// find every spend ConfigMap with one labeled list.
func TestBudgetPublisher_PublishSetsSpendLabel(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	client := k8sfake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: AgentSpendConfigMapName("prov"), Namespace: "kaalm-system"}},
	)
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", "agent/test", 42)
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}
	pub.publish(ctx, p, nil)
	for name, want := range map[string]string{
		BudgetConfigMapName("prov"):     SpendLabelBudget,
		AgentSpendConfigMapName("prov"): SpendLabelAgentSpend,
	} {
		cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := cm.Labels[SpendConfigMapLabel]; got != want {
			t.Errorf("%s label %s = %q, want %q", name, SpendConfigMapLabel, got, want)
		}
	}
}

// spendCM builds a spend ConfigMap in the operator namespace; label is the
// spend label value, or "" for a ConfigMap written before the label existed.
func spendCM(name, label string, data map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kaalm-system"},
		Data:       data,
	}
	if label != "" {
		cm.Labels = map[string]string{SpendConfigMapLabel: label}
	}
	return cm
}

// partialRaw is a current-period partial holding one key.
func partialRaw(t *testing.T, key, usd string) string {
	t.Helper()
	raw, err := json.Marshal(budgetPartial{Period: PeriodKey("monthly", time.Now()), Spend: map[string]string{key: usd}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// configMapActions returns the verbs of the clientset's ConfigMap requests,
// with the object name for get and patch.
func configMapActions(client *k8sfake.Clientset) []string {
	var out []string
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "configmaps" {
			continue
		}
		named, ok := a.(interface{ GetName() string })
		if ok && (a.GetVerb() == "get" || a.GetVerb() == "patch") {
			out = append(out, a.GetVerb()+" "+named.GetName())
			continue
		}
		out = append(out, a.GetVerb())
	}
	return out
}

// foldState reads a provider's ledger fields under the ledger lock.
func foldState(ledger *BudgetLedger, name string) (lastFoldOK, dirtySince time.Time, peers, peersW map[string]float64) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	l := ledger.providers[name]
	if l == nil {
		return time.Time{}, time.Time{}, nil, nil
	}
	return l.lastFoldOK, l.dirtySince, l.peers, l.peersW
}

// The tick reads every provider's budget and agent-spend ConfigMaps with
// one labeled list and no GET, for a provider with ConfigMaps and one with
// none, and both are folded with their read-path liveness stamped.
func TestBudgetPublisher_TickReadsWithOneList(t *testing.T) {
	p := budgetProvider()
	other := budgetProvider()
	other.Name = "other"
	objs := []*corev1.ConfigMap{
		withRV(spendCM(BudgetConfigMapName("prov"), SpendLabelBudget, map[string]string{"gw-1": partialRaw(t, "team-a", "70.00")}), "3"),
		withRV(spendCM(AgentSpendConfigMapName("prov"), SpendLabelAgentSpend, map[string]string{"gw-1": partialRaw(t, "team-a/agent/x", "5.00")}), "4"),
	}
	client := k8sfake.NewSimpleClientset(objs[0], objs[1])
	cache := crfake.NewClientBuilder().WithObjects(objs[0].DeepCopy(), objs[1].DeepCopy()).Build()
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Cache: cache, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Providers: providersFn(p, other)}

	pub.tick(context.Background())
	if got := configMapActions(client); len(got) != 1 || got[0] != "list" {
		t.Errorf("ConfigMap requests = %q, want one list", got)
	}
	foldOK, _, peers, peersW := foldState(ledger, "prov")
	if foldOK.IsZero() || peers["team-a"] != 70 || peersW["team-a/agent/x"] != 5 {
		t.Errorf("prov: lastFoldOK %v, peers %v, workload peers %v; want stamped, 70, 5", foldOK, peers, peersW)
	}
	if foldOK, _, _, _ := foldState(ledger, "other"); foldOK.IsZero() {
		t.Error("a provider with no ConfigMap must still be folded (empty) and stamped")
	}
}

// A failed list folds nothing: the read-path liveness stays unstamped, so
// hard admission inside the boundary region fails closed. The partial is
// still published.
func TestBudgetPublisher_TickListFailureLeavesFoldUnstamped(t *testing.T) {
	mp := hardProvider(blockAt100())
	client := k8sfake.NewSimpleClientset(spendCM(BudgetConfigMapName("prov"), "", nil))
	client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list refused")
	})
	ledger := NewBudgetLedger()
	ledger.Add(mp, "team-a", "agent/test", 97)
	pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
		PodName: "gw-0", Providers: providersFn(mp)}

	pub.tick(context.Background())
	if foldOK, _, _, _ := foldState(ledger, "prov"); !foldOK.IsZero() {
		t.Errorf("lastFoldOK = %v after a failed list, want unstamped", foldOK)
	}
	d, settle := ledger.Admit(mp, "team-a", "agent/test")
	if settle != nil {
		settle(0)
	}
	if !d.Unavailable {
		t.Errorf("Admit = %+v, want Unavailable (fail closed)", d)
	}
	if !slices.Contains(configMapActions(client), "patch "+BudgetConfigMapName("prov")) {
		t.Errorf("ConfigMap requests = %q, want the partial applied", configMapActions(client))
	}
}

// A partial the list shows already stored, under the expected label, is not
// re-applied, and the write-path signal still clears. A different stored
// value, or a wrong label value, is applied.
func TestBudgetPublisher_TickSkipsUnchangedPartial(t *testing.T) {
	at := time.Date(2099, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		budgetLabel string
		mutate      func(budgetRaw string) string
		wantPatch   bool
	}{
		{"unchanged", SpendLabelBudget, func(raw string) string { return raw }, false},
		{"value differs", SpendLabelBudget, func(string) string { return `{"period":"2099-06","team-a":"1.00"}` }, true},
		{"wrong label value", "other", func(raw string) string { return raw }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := budgetProvider()
			ledger := NewBudgetLedger()
			ledger.now = func() time.Time { return at }
			ledger.Add(mp, "team-a", "agent/x", 12.34)
			period, spend, margin, _ := ledger.OwnPartial(mp)
			raw, _ := json.Marshal(budgetPartial{Period: period, Spend: spend, MarginExceeded: margin, ProviderUID: string(mp.UID)})
			wperiod, wspend, _ := ledger.OwnWorkloadPartial(mp)
			wraw, _ := json.Marshal(budgetPartial{Period: wperiod, Spend: wspend, ProviderUID: string(mp.UID)})
			client := k8sfake.NewSimpleClientset(
				spendCM(BudgetConfigMapName("prov"), tc.budgetLabel, map[string]string{"gw-0": tc.mutate(string(raw))}),
				spendCM(AgentSpendConfigMapName("prov"), SpendLabelAgentSpend, map[string]string{"gw-0": string(wraw)}),
			)
			pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
				PodName: "gw-0", Providers: providersFn(mp), now: func() time.Time { return at }}

			pub.tick(context.Background())
			patched := slices.Contains(configMapActions(client), "patch "+BudgetConfigMapName("prov"))
			if patched != tc.wantPatch {
				t.Errorf("budget partial applied = %v, want %v (requests %q)", patched, tc.wantPatch, configMapActions(client))
			}
			if slices.Contains(configMapActions(client), "patch "+AgentSpendConfigMapName("prov")) {
				t.Errorf("unchanged workload partial was re-applied (requests %q)", configMapActions(client))
			}
			if _, dirty, _, _ := foldState(ledger, "prov"); !dirty.IsZero() {
				t.Errorf("dirtySince = %v after the tick, want cleared", dirty)
			}
		})
	}
}

// Upgrade from 2.0.0: a budget ConfigMap without the spend label is missing
// from the labeled list, but it exists, so the tick reads it with a live GET
// and hard enforcement counts the same peer spend as before. The watch
// cache only tells that the name exists; its (older) content is never
// folded. With no cache, the GET runs for every name the list misses.
func TestBudgetPublisher_TickReadsUnlabeledConfigMapLive(t *testing.T) {
	for _, withCache := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache=%v", withCache), func(t *testing.T) {
			mp := hardProvider(blockAt100())
			name := BudgetConfigMapName("prov")
			client := k8sfake.NewSimpleClientset(spendCM(name, "", map[string]string{"gw-1": partialRaw(t, "team-a", "97.00")}))
			ledger := NewBudgetLedger()
			pub := &BudgetPublisher{Client: client, Ledger: ledger, OperatorNamespace: "kaalm-system",
				PodName: "gw-0", Providers: providersFn(mp)}
			if withCache {
				pub.Cache = crfake.NewClientBuilder().WithObjects(
					spendCM(name, "", map[string]string{"gw-1": partialRaw(t, "team-a", "10.00")})).Build()
			}

			pub.tick(context.Background())
			foldOK, _, peers, _ := foldState(ledger, "prov")
			if foldOK.IsZero() || peers["team-a"] != 97 {
				t.Errorf("lastFoldOK %v, peers %v; want stamped with the live 97", foldOK, peers)
			}
			gets := 0
			for _, a := range configMapActions(client) {
				if a == "get "+name {
					gets++
				}
			}
			if gets != 1 {
				t.Errorf("GETs of %s = %d, want 1 (requests %q)", name, gets, configMapActions(client))
			}
			d, settle := ledger.Admit(mp, "team-a", "agent/test")
			if settle != nil {
				settle(0)
			}
			if !d.BoundaryEngaged || d.Unavailable {
				t.Errorf("Admit = %+v, want BoundaryEngaged at 97%% peer spend", d)
			}
		})
	}
}

// The startup seed reads a labeled ConfigMap from the list, with no GET.
func TestBudgetPublisher_SeedFromCanonicalReadsTheList(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	canonical, _ := json.Marshal(map[string]string{"team-a": "130"})
	cm := withRV(spendCM(BudgetConfigMapName("prov"), SpendLabelBudget, map[string]string{CanonicalKey: string(canonical)}), "3")
	client := k8sfake.NewSimpleClientset(cm)
	ledger := NewBudgetLedger()
	pub := &BudgetPublisher{Client: client, Cache: crfake.NewClientBuilder().WithObjects(cm.DeepCopy()).Build(),
		Ledger: ledger, OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}

	pub.SeedFromCanonical(context.Background())
	if d := ledger.Enforce(p, "team-a"); d.Action != kaalmv1beta1.BudgetActionBlock {
		t.Errorf("canonical seed of 130%% should block, got %+v", d)
	}
	for _, a := range configMapActions(client) {
		if strings.HasPrefix(a, "get ") {
			t.Errorf("ConfigMap requests = %q, want no GET", configMapActions(client))
			break
		}
	}
}

// trackerCache is a ConfigMap watch cache that is always caught up: Get
// reads the fake clientset's current object, resourceVersion included.
type trackerCache struct {
	client.Reader
	clientset *k8sfake.Clientset
}

func (c trackerCache) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	got, err := c.clientset.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), key.Namespace, key.Name)
	if err != nil {
		return err
	}
	got.(*corev1.ConfigMap).DeepCopyInto(obj.(*corev1.ConfigMap))
	return nil
}

// withRV sets a ConfigMap's resourceVersion; the fake clientset stores it
// as given.
func withRV(cm *corev1.ConfigMap, rv string) *corev1.ConfigMap {
	cm.ResourceVersion = rv
	return cm
}

// A peer publishes provider prov's partial after the tick's list and the
// watch folds it. The tick must not then fold prov from the older listed
// copy: the watch cache's resourceVersion differs from the listed one, so
// the tick reads prov's ConfigMap live. The peer's publish happens during
// the tick's attempt to apply an earlier provider, aaa. The fake clientset
// rejects that apply (kaalm-budget-aaa does not exist), but the patch
// reactor runs first, which is all this test needs.
func TestBudgetPublisher_TickNeverFoldsAStaleListedCopy(t *testing.T) {
	first := hardProvider(blockAt100())
	first.Name = "aaa"
	p := hardProvider(blockAt100())
	name := BudgetConfigMapName("prov")
	client := k8sfake.NewSimpleClientset(withRV(spendCM(name, SpendLabelBudget,
		map[string]string{"gw-1": partialRaw(t, "team-a", "50.00")}), "1"))
	ledger := NewBudgetLedger()
	ledger.Add(first, "team-a", "agent/test", 1) // so the tick attempts aaa's apply
	done := false
	client.PrependReactor("patch", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if done || a.(k8stesting.PatchAction).GetName() != BudgetConfigMapName("aaa") {
			return false, nil, nil
		}
		done = true
		// The peer settles and publishes 99 for prov; the watch folds it.
		peer := withRV(spendCM(name, SpendLabelBudget, map[string]string{"gw-1": partialRaw(t, "team-a", "99.00")}), "2")
		if err := client.Tracker().Update(a.GetResource(), peer, "kaalm-system"); err != nil {
			t.Fatal(err)
		}
		ledger.FoldPeers(p, map[string]float64{"team-a": 99})
		return false, nil, nil
	})
	pub := &BudgetPublisher{Client: client, Cache: trackerCache{clientset: client}, Ledger: ledger,
		OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(first, p)}

	pub.tick(context.Background())
	if !done {
		t.Fatal("the tick never attempted aaa's apply; the peer publish never ran")
	}
	foldOK, _, peers, _ := foldState(ledger, "prov")
	if foldOK.IsZero() || peers["team-a"] != 99 {
		t.Errorf("prov: lastFoldOK %v, peers %v; want stamped with the newer 99, not the listed 50", foldOK, peers)
	}
}

// A listed ConfigMap is folded from the list only when the watch cache
// holds the same resourceVersion. A different resourceVersion, or a
// ConfigMap the cache does not hold yet, is read live.
func TestBudgetPublisher_TickChecksListedCopyAgainstCache(t *testing.T) {
	cases := []struct {
		name    string
		cached  *corev1.ConfigMap // nil: the cache does not hold it
		wantGet bool
	}{
		{"same resourceVersion", withRV(spendCM(BudgetConfigMapName("prov"), SpendLabelBudget, nil), "7"), false},
		{"different resourceVersion", withRV(spendCM(BudgetConfigMapName("prov"), SpendLabelBudget, nil), "6"), true},
		{"not in the cache", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := budgetProvider()
			name := BudgetConfigMapName("prov")
			client := k8sfake.NewSimpleClientset(withRV(spendCM(name, SpendLabelBudget,
				map[string]string{"gw-1": partialRaw(t, "team-a", "70.00")}), "7"))
			builder := crfake.NewClientBuilder()
			if tc.cached != nil {
				builder = builder.WithObjects(tc.cached)
			}
			ledger := NewBudgetLedger()
			pub := &BudgetPublisher{Client: client, Cache: builder.Build(), Ledger: ledger,
				OperatorNamespace: "kaalm-system", PodName: "gw-0", Providers: providersFn(p)}

			pub.tick(context.Background())
			if got := slices.Contains(configMapActions(client), "get "+name); got != tc.wantGet {
				t.Errorf("live GET of %s = %v, want %v (requests %q)", name, got, tc.wantGet, configMapActions(client))
			}
			if foldOK, _, peers, _ := foldState(ledger, "prov"); foldOK.IsZero() || peers["team-a"] != 70 {
				t.Errorf("lastFoldOK %v, peers %v; want stamped, 70", foldOK, peers)
			}
		})
	}
}
