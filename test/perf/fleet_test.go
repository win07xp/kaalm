//go:build perftest

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

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestFleetNamesAndPlacement(t *testing.T) {
	f := fleet{phase: "p", prefix: "spread-", target: 5, namespaces: []string{"a", "b", "c"}}
	if got := f.name(7); got != "spread-0007" {
		t.Errorf("name(7) = %q", got)
	}
	want := []string{"a", "b", "c", "a", "b"}
	for i, ns := range want {
		if got := f.namespaceOf(i); got != ns {
			t.Errorf("namespaceOf(%d) = %q, want %q", i, got, ns)
		}
	}
	if f.scope() != "" {
		t.Errorf("a spread fleet lists every namespace, got scope %q", f.scope())
	}
	one := fleet{namespaces: []string{"perf"}}
	if one.scope() != "perf" || one.namespaceOf(42) != "perf" {
		t.Errorf("a one-namespace fleet stays in it: scope %q, namespaceOf %q", one.scope(), one.namespaceOf(42))
	}
}

func TestSpreadNamespaces(t *testing.T) {
	got := spreadNamespaces("perf", 20)
	if len(got) != 20 || got[0] != "perf-00" || got[9] != "perf-09" || got[19] != "perf-19" {
		t.Errorf("spreadNamespaces(perf, 20) = %v", got)
	}
}

func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("canceled context: waited %s, want an early return", took)
	}

	start = time.Now()
	if err := sleepCtx(context.Background(), 10*time.Millisecond); err != nil {
		t.Errorf("background context: err = %v", err)
	}
	if took := time.Since(start); took < 10*time.Millisecond {
		t.Errorf("background context: returned after %s, want the full 10ms", took)
	}
}

func TestTrafficSettled(t *testing.T) {
	channel := func(reason string) kaalmv1beta1.AgentChannel {
		var ch kaalmv1beta1.AgentChannel
		if reason != "" {
			meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
				Type:   kaalmv1beta1.ConditionPlatformConnected,
				Status: metav1.ConditionUnknown,
				Reason: reason,
			})
		}
		return ch
	}
	cases := []struct {
		name string
		ch   kaalmv1beta1.AgentChannel
		want bool
	}{
		{"no condition yet", channel(""), false},
		{"no recent traffic", channel(kaalmv1beta1.ReasonNoRecentTraffic), true},
		{"webhook ready", channel(kaalmv1beta1.ReasonWebhookReady), false},
	}
	for _, c := range cases {
		if got := trafficSettled(&c.ch); got != c.want {
			t.Errorf("%s: trafficSettled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestChannelNamesAndSecretPairs(t *testing.T) {
	if got := channelName("ramp-0001", 0); got != "ramp-0001" {
		t.Errorf("slot 0 = %q, want the agent's name", got)
	}
	if got := channelName("ramp-0001", 2); got != "ramp-0001-c2" {
		t.Errorf("slot 2 = %q", got)
	}
	if hook, hmac := secretPairNames(7); hook != "perf-hook-0007" || hmac != "perf-callback-hmac-0007" {
		t.Errorf("secretPairNames(7) = %q, %q", hook, hmac)
	}
	for _, c := range []struct{ channels, n, want int }{{400, 0, 2}, {400, 200, 400}, {400, 500, 800}} {
		if got := referencedSecrets(c.channels, c.n); got != c.want {
			t.Errorf("referencedSecrets(%d, %d) = %d, want %d", c.channels, c.n, got, c.want)
		}
	}
	f := fleet{prefix: "ramp-"}
	if i, ok := f.index("ramp-0042"); !ok || i != 42 {
		t.Errorf("index(ramp-0042) = %d, %v", i, ok)
	}
	for _, name := range []string{"loadgen-agent", "ramp-42", "churn-0001"} {
		if _, ok := f.index(name); ok {
			t.Errorf("index(%s) parsed; want not a fleet agent", name)
		}
	}
}

func TestFleetChannelsSharedByDefault(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	cb := "https://mock-provider.perf.svc:8443/callback"
	got := f.channels(3, config{ChannelsPerAgent: 1}, cb)
	if len(got) != 1 {
		t.Fatalf("%d channels, want 1", len(got))
	}
	want := channelObj("perf", "ramp-0003", phaseRamp, cb)
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("default channel differs from channelObj:\n got %+v\nwant %+v", got[0], want)
	}
}

func TestFleetChannelsSpreadOverPairs(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	got := f.channels(2, config{ChannelsPerAgent: 2, ChannelSecrets: 3}, "https://cb")
	want := []struct{ name, path, hook, hmac string }{
		{"ramp-0002", "/channels/perf/ramp-0002", "perf-hook-0001", "perf-callback-hmac-0001"},
		{"ramp-0002-c1", "/channels/perf/ramp-0002-c1", "perf-hook-0002", "perf-callback-hmac-0002"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d channels, want %d", len(got), len(want))
	}
	for i, w := range want {
		ch := got[i].(*kaalmv1beta1.AgentChannel)
		wh := ch.Spec.Webhook
		if ch.Name != w.name || ch.Namespace != "perf" || wh.Path != w.path {
			t.Errorf("channel %d: %s/%s at %s, want perf/%s at %s", i, ch.Namespace, ch.Name, wh.Path, w.name, w.path)
		}
		if ch.Spec.AgentRef.Name != "ramp-0002" || ch.Labels[phaseLabel] != phaseRamp {
			t.Errorf("channel %d: agentRef %q, phase label %q", i, ch.Spec.AgentRef.Name, ch.Labels[phaseLabel])
		}
		if ref := wh.Auth.SecretRef; ref.Name != w.hook || ref.Key != hookSecretKey {
			t.Errorf("channel %d: bearer ref %s/%s, want %s/%s", i, ref.Name, ref.Key, w.hook, hookSecretKey)
		}
		if ref := wh.CallbackAuth.HMAC.SecretRef; ref.Name != w.hmac || ref.Key != "key" {
			t.Errorf("channel %d: HMAC ref %s/%s, want %s/key", i, ref.Name, ref.Key, w.hmac)
		}
	}
}

func TestChannelSecretObjsCarryTheOptInLabel(t *testing.T) {
	hook := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte("t")}}
	hmac := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"key": []byte("k")}}
	objs := channelSecretObjs("perf", phaseRamp, 3, hook, hmac)
	if len(objs) != 6 {
		t.Fatalf("%d Secrets, want 6", len(objs))
	}
	for i, o := range objs {
		sec := o.(*corev1.Secret)
		wantHook, wantHMAC := secretPairNames(i / 2)
		wantName, src := wantHook, hook
		if i%2 == 1 {
			wantName, src = wantHMAC, hmac
		}
		if sec.Name != wantName || sec.Namespace != "perf" {
			t.Errorf("Secret %d: %s/%s, want perf/%s", i, sec.Namespace, sec.Name, wantName)
		}
		if sec.Labels["kaalm.io/channel-credential"] != "true" || sec.Labels[phaseLabel] != phaseRamp {
			t.Errorf("Secret %s: labels %v", sec.Name, sec.Labels)
		}
		if sec.Type != src.Type || !reflect.DeepEqual(sec.Data, src.Data) {
			t.Errorf("Secret %s: type %s data %v, want a copy of the infra Secret", sec.Name, sec.Type, sec.Data)
		}
	}
}

func fakeFleetClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, kaalmv1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestDeleteAgentsRemovesEveryChannelSlot(t *testing.T) {
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	cfg := config{ChannelsPerAgent: 2}
	var seed []client.Object
	names := map[string]string{}
	for i := 0; i < 2; i++ {
		seed = append(seed, agentObj("perf", f.name(i), activeClassName, "img", phaseRamp, false))
		seed = append(seed, f.channels(i, cfg, "https://cb")...)
		names[f.name(i)] = "perf"
	}
	c := fakeFleetClient(t, seed...)
	k := &cluster{c: c}
	if err := k.deleteAgents(context.Background(), names, cfg.ChannelsPerAgent, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var channels kaalmv1beta1.AgentChannelList
	var agents kaalmv1beta1.AgentList
	if err := c.List(context.Background(), &channels); err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(channels.Items)+len(agents.Items) != 0 {
		t.Errorf("left behind: %d channels, %d agents", len(channels.Items), len(agents.Items))
	}
}

func TestDeletePhaseRemovesLabeledSecretsOnly(t *testing.T) {
	hook := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "perf", Name: hookSecretName}}
	seed := append([]client.Object{hook}, channelSecretObjs("perf", phaseRamp, 1, hook, hook)...)
	c := fakeFleetClient(t, seed...)
	k := &cluster{c: c}
	if _, err := k.deletePhase(context.Background(), []string{"perf"}, phaseRamp, "ramp-", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var secrets corev1.SecretList
	if err := c.List(context.Background(), &secrets); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 || secrets.Items[0].Name != hookSecretName {
		var left []string
		for i := range secrets.Items {
			left = append(left, secrets.Items[i].Name)
		}
		t.Errorf("Secrets after teardown %v, want only the unlabeled %s", left, hookSecretName)
	}
}
