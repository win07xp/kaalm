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

package console

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// envClient is a real-apiserver client, nil when envtest is unavailable
// (plain `go test` without the assets; make test provides them).
var envClient client.Client

func TestMain(m *testing.M) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		// The data-layer envtest skips without assets (plain `go test`);
		// make test / cover-check provide them. Print why so a skip in CI is
		// diagnosable rather than silent.
		_, _ = os.Stderr.WriteString("console envtest unavailable: " + err.Error() + "\n")
	}
	if err == nil {
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			panic(err)
		}
		if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
			panic(err)
		}
		envClient, err = client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			panic("client: " + err.Error())
		}
	}
	code := m.Run()
	if cfg != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

// TestData_AgainstRealAPIServer proves the data layer against a real
// apiserver: the CRD schemas round-trip the status fields the DTOs map, and
// list-by-namespace behaves as the fake cannot fully prove.
func TestData_AgainstRealAPIServer(t *testing.T) {
	if envClient == nil {
		t.Skip("envtest assets unavailable")
	}
	ctx := context.Background()
	if err := envClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "console-e2e"}}); err != nil {
		t.Fatal(err)
	}

	agent := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "walker", Namespace: "console-e2e"},
		Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "standard"}},
	}
	if err := envClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	agent.Status = kaalmv1beta1.AgentStatus{
		Phase:        kaalmv1beta1.AgentHibernated,
		HibernatedAt: &now,
		Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Hibernated",
			LastTransitionTime: now,
		}},
	}
	if err := envClient.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}

	provider := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "console-e2e-prov"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:              "openai",
			Endpoint:          "https://api.example.com",
			CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: "k", Key: "key"},
			AllowedNamespaces: []string{"*"},
			Budget:            kaalmv1beta1.ModelProviderBudget{PerNamespaceUSD: "50.00", Period: "monthly"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
		},
	}
	if err := envClient.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.BudgetUsage = []kaalmv1beta1.ModelProviderBudgetUsage{
		{Namespace: "console-e2e", Period: "monthly", SpentUSD: "1.25", PercentUsed: 2, State: "Normal"},
	}
	if err := envClient.Status().Update(ctx, provider); err != nil {
		t.Fatal(err)
	}

	d := &Data{Reader: envClient}
	rows, err := d.Fleet(ctx, "console-e2e")
	if err != nil || len(rows) != 1 {
		t.Fatalf("fleet = %+v, %v", rows, err)
	}
	if rows[0].Phase != "Hibernated" || rows[0].Ready || rows[0].HibernatedAt == nil {
		t.Errorf("fleet row = %+v", rows[0])
	}

	spend, err := d.Spend(ctx, "console-e2e")
	if err != nil || len(spend) != 1 {
		t.Fatalf("spend = %+v, %v", spend, err)
	}
	if spend[0].SpentUSD != "1.25" || spend[0].CeilingUSD != "50.00" {
		t.Errorf("spend row = %+v", spend[0])
	}

	// Namespace scoping: another namespace sees no fleet.
	empty, err := d.Fleet(ctx, "default")
	if err != nil || len(empty) != 0 {
		t.Errorf("default fleet = %+v", empty)
	}
}
