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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// TestAgentSpendReducer proves the reducer's whole pass against the real
// apiserver: pruned replicas fold into _retired, stale periods drop, and
// _canonical carries live plus retired for fresh-replica seeding.
func TestAgentSpendReducer(t *testing.T) {
	ctx := context.Background()
	currentPeriod := gateway.PeriodKey("monthly", time.Now())
	stalePeriod := "2020-01"

	partial := func(period string, spend map[string]string) string {
		flat := map[string]string{"period": period}
		for k, v := range spend {
			flat[k] = v
		}
		raw, _ := json.Marshal(flat)
		return string(raw)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.AgentSpendConfigMapName("spend-prov"),
			Namespace: testSystemNamespace,
		},
		Data: map[string]string{
			"gw-live":          partial(currentPeriod, map[string]string{"team-a/agent/sup": "10.00"}),
			"gw-dead":          partial(currentPeriod, map[string]string{"team-a/agent/sup": "2.00", "team-a/task/fix": "1.00"}),
			"gw-stale":         partial(stalePeriod, map[string]string{"team-a/agent/old": "99.00"}),
			gateway.RetiredKey: partial(currentPeriod, map[string]string{"team-a/agent/sup": "0.50"}),
		},
	}
	if err := testClient.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, cm) })

	// The reducer reads through the manager cache; wait for the Create to
	// land there before reducing, or the reducer's Get sees NotFound (in
	// production that race is benign: the next one-minute requeue reduces).
	key := types.NamespacedName{Namespace: testSystemNamespace, Name: gateway.AgentSpendConfigMapName("spend-prov")}
	eventually(t, func() error {
		var check corev1.ConfigMap
		if err := testCache.Get(ctx, key, &check); err != nil {
			return err
		}
		if len(check.Data) != 4 {
			return fmt.Errorf("cache holds %d keys, want 4", len(check.Data))
		}
		return nil
	})

	mp := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "spend-prov"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://api.example.com",
			CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: "k", Key: "key"},
			AllowedNamespaces: []string{"*"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
			Budget:            kaalmv1beta1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"},
		},
	}

	r := &ModelProviderReconciler{Client: testClient, OperatorNamespace: testSystemNamespace}
	live := map[string]bool{"gw-live": true}
	if err := r.reconcileAgentSpend(ctx, mp, live); err != nil {
		t.Fatal(err)
	}

	// Wait for the reducer's Update to land back in the cache.
	var got corev1.ConfigMap
	eventually(t, func() error {
		if err := testCache.Get(ctx, key, &got); err != nil {
			return err
		}
		if _, dead := got.Data["gw-dead"]; dead {
			return fmt.Errorf("gw-dead still in the cached view")
		}
		return nil
	})

	if _, ok := got.Data["gw-dead"]; ok {
		t.Error("pruned replica key must be deleted")
	}
	if _, ok := got.Data["gw-stale"]; ok {
		t.Error("stale-period key must be deleted")
	}
	if _, ok := got.Data["gw-live"]; !ok {
		t.Error("live replica key must survive")
	}

	// The dead replica's current-period spend folded into _retired on top of
	// the existing accumulator.
	period, retired, _, _, err := gateway.ParseBudgetPartial(got.Data[gateway.RetiredKey])
	if err != nil || period != currentPeriod {
		t.Fatalf("_retired = %q err %v", got.Data[gateway.RetiredKey], err)
	}
	if retired["team-a/agent/sup"] != 2.5 || retired["team-a/task/fix"] != 1 {
		t.Errorf("retired = %+v", retired)
	}

	// _canonical = live + retired, ready for fresh-replica seeding.
	var canonical map[string]string
	if err := json.Unmarshal([]byte(got.Data[gateway.CanonicalKey]), &canonical); err != nil {
		t.Fatal(err)
	}
	if canonical["team-a/agent/sup"] != "12.50" || canonical["team-a/task/fix"] != "1.00" {
		t.Errorf("canonical = %+v", canonical)
	}
	if _, leaked := canonical["team-a/agent/old"]; leaked {
		t.Error("stale-period spend must not enter canonical")
	}

	// A second pass with nothing changed writes nothing (no Update churn):
	// prove idempotence by comparing resourceVersion across a repeat run.
	before := got.ResourceVersion
	if err := r.reconcileAgentSpend(ctx, mp, live); err != nil {
		t.Fatal(err)
	}
	if err := testAPIReader.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != before {
		t.Error("an unchanged pass must not update the ConfigMap")
	}
}

// The agent-spend reducer drops keys written for a deleted provider of the
// same name, the same way the budget reducer does.
func TestModelProvider_AgentSpendDropsOtherIncarnation(t *testing.T) {
	ctx := context.Background()
	period := gateway.PeriodKey("monthly", time.Now())
	tagged := func(usd string) string {
		return fmt.Sprintf(`{"period":%q,"team-a/agent/x":%q,"_providerUID":"uid-old"}`, period, usd)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.AgentSpendConfigMapName("inc"), Namespace: testOperatorNamespace},
		Data: map[string]string{
			"gw-0":             tagged("90.00"),
			"gw-1":             fmt.Sprintf(`{"period":%q,"team-a/agent/x":"5.00"}`, period),
			gateway.RetiredKey: tagged("7.00"),
			"gw-9":             tagged("3.00"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm).Build()
	r := &ModelProviderReconciler{Client: c, OperatorNamespace: testOperatorNamespace}
	mp := eventsProvider("inc", func(mp *kaalmv1beta1.ModelProvider) {
		mp.UID = "uid-new"
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
	})
	if err := r.reconcileAgentSpend(ctx, mp, map[string]bool{"gw-0": true, "gw-1": true}); err != nil {
		t.Fatal(err)
	}
	var got corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &got); err != nil {
		t.Fatal(err)
	}
	for k, v := range got.Data {
		if strings.Contains(v, "uid-old") {
			t.Errorf("key %s written for the old provider survived: %s", k, v)
		}
	}
	if got.Data[gateway.CanonicalKey] != `{"team-a/agent/x":"5.00"}` {
		t.Errorf("_canonical = %s, want only the untagged 5.00", got.Data[gateway.CanonicalKey])
	}
}
