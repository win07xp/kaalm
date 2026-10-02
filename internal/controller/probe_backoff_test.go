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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func healthyCond(status metav1.ConditionStatus, since time.Time) []metav1.Condition {
	return []metav1.Condition{{
		Type: kaalmv1beta1.ConditionHealthy, Status: status, Reason: "x",
		LastTransitionTime: metav1.NewTime(since),
	}}
}

// The failing-probe requeue doubles with each periodic failure: the delay is
// the interval plus the time the provider has been failing, read from the
// Healthy condition's lastTransitionTime so a restart keeps it.
func TestProbeRequeue_DoublesWhileFailing(t *testing.T) {
	now := time.Now()
	const i = time.Minute
	cases := []struct {
		name  string
		conds []metav1.Condition
		want  time.Duration
	}{
		{"no condition", nil, i},
		{"healthy", healthyCond(metav1.ConditionTrue, now.Add(-time.Hour)), i},
		{"unknown", healthyCond(metav1.ConditionUnknown, now.Add(-time.Hour)), i},
		{"first failure", healthyCond(metav1.ConditionFalse, now), i},
		{"second failure", healthyCond(metav1.ConditionFalse, now.Add(-i)), 2 * i},
		{"third failure", healthyCond(metav1.ConditionFalse, now.Add(-3*i)), 4 * i},
		{"fourth failure", healthyCond(metav1.ConditionFalse, now.Add(-7*i)), 8 * i},
		{"capped at ten intervals or ten minutes", healthyCond(metav1.ConditionFalse, now.Add(-15*i)), 10 * time.Minute},
		{"future transition time", healthyCond(metav1.ConditionFalse, now.Add(time.Hour)), i},
	}
	for _, tc := range cases {
		if got := probeRequeue(tc.conds, i, now); got != tc.want {
			t.Errorf("%s: probeRequeue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The cap is ten intervals or ten minutes, whichever is smaller, and never
// shorter than the interval itself.
func TestProbeRequeue_Cap(t *testing.T) {
	now := time.Now()
	longAgo := healthyCond(metav1.ConditionFalse, now.Add(-24*time.Hour))
	cases := []struct {
		interval, want time.Duration
	}{
		{30 * time.Second, 5 * time.Minute},
		{time.Minute, 10 * time.Minute},
		{5 * time.Minute, 10 * time.Minute},
		{time.Hour, time.Hour},
	}
	for _, tc := range cases {
		if got := probeRequeue(longAgo, tc.interval, now); got != tc.want {
			t.Errorf("interval %v: capped delay = %v, want %v", tc.interval, got, tc.want)
		}
	}
}

// near allows for the second precision a condition's lastTransitionTime
// keeps once it is serialized.
func near(got, want time.Duration) bool {
	d := got - want
	return d > -2*time.Second && d < 2*time.Second
}

// probedProvider builds a ModelProvider that has already carried its
// finalizer, with a Healthy condition that went False at since.
func probedProvider(name string, healthy metav1.ConditionStatus, since time.Time) *kaalmv1beta1.ModelProvider {
	return &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Generation: 1,
			Finalizers: []string{kaalmv1beta1.ProviderFinalizer},
		},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://api.example.com",
			CredentialsRef:    kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
			AllowedNamespaces: []string{"*"},
		},
		Status: kaalmv1beta1.ModelProviderStatus{Conditions: healthyCond(healthy, since)},
	}
}

func providerKey(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: providerCredentialMeta(name+"-key", testOperatorNamespace),
		Data:       map[string][]byte{"token": []byte("sk-test")},
	}
}

// A ModelProvider that keeps failing its probe requeues on the backoff, and
// one probe success brings it back to the interval.
func TestModelProvider_FailedProbeBacksOff(t *testing.T) {
	ctx := context.Background()
	mp := probedProvider("mp-backoff", metav1.ConditionFalse, time.Now().Add(-3*time.Minute))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(mp, providerKey("mp-backoff")).WithStatusSubresource(mp).Build()
	health := newFakeHealth()
	r := &ModelProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(10),
		OperatorNamespace: testOperatorNamespace, Health: health,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "mp-backoff"}}

	health.set("mp-backoff", ProviderProbeResult{Err: errString("upstream 503")})
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !near(res.RequeueAfter, 4*time.Minute) {
		t.Errorf("failing for 3m at a 1m interval: RequeueAfter = %v, want about 4m", res.RequeueAfter)
	}

	health.set("mp-backoff", ProviderProbeResult{AuthFailed: true})
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !near(res.RequeueAfter, 4*time.Minute) {
		t.Errorf("a rejected credential backs off too: RequeueAfter = %v, want about 4m", res.RequeueAfter)
	}

	health.set("mp-backoff", ProviderProbeResult{Healthy: true})
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("after a success: RequeueAfter = %v, want the 1m interval", res.RequeueAfter)
	}
}

// The ToolProvider probe backs off the same way.
func TestToolProvider_FailedProbeBacksOff(t *testing.T) {
	ctx := context.Background()
	tp := &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-backoff", Generation: 1,
			Finalizers: []string{kaalmv1beta1.ToolProviderFinalizer},
		},
		Spec: kaalmv1beta1.ToolProviderSpec{
			Type: "mcp", Endpoint: "https://mcp.example.com",
			HealthCheck: &kaalmv1beta1.ToolProviderHealthCheck{Enabled: true, IntervalSeconds: 30},
		},
		Status: kaalmv1beta1.ToolProviderStatus{
			Conditions: healthyCond(metav1.ConditionFalse, time.Now().Add(-time.Minute)),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp).WithStatusSubresource(tp).Build()
	health := newFakeToolHealth()
	r := &ToolProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(10),
		OperatorNamespace: testOperatorNamespace, Health: health,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "tp-backoff"}}

	health.set("tp-backoff", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Err: errString("connection refused")}})
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !near(res.RequeueAfter, 90*time.Second) {
		t.Errorf("failing for 1m at a 30s interval: RequeueAfter = %v, want about 90s", res.RequeueAfter)
	}

	health.set("tp-backoff", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true}})
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("after a success: RequeueAfter = %v, want the 30s interval", res.RequeueAfter)
	}
}
