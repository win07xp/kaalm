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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const orphanTestNS = "kaalm-system"

var orphanTestNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// asyncRecord builds a kaalm-async-* record. An empty channel name leaves the
// channel labels off; an empty expiry leaves the annotation off.
func asyncRecord(name, chNS, chName, expiresAt string, created time.Time) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: orphanTestNS,
		CreationTimestamp: metav1.NewTime(created),
		Labels:            map[string]string{},
		Annotations:       map[string]string{},
	}}
	if chName != "" {
		cm.Labels[kaalmv1beta1.LabelChannelNamespace] = chNS
		cm.Labels[kaalmv1beta1.LabelChannelName] = chName
	}
	if expiresAt != "" {
		cm.Annotations[kaalmv1beta1.AnnotationExpiresAt] = expiresAt
	}
	return cm
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func newOrphanPruner(t *testing.T, objs ...client.Object) (*AsyncOrphanPruner, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &AsyncOrphanPruner{
		Client: c, OperatorNamespace: orphanTestNS,
		now: func() time.Time { return orphanTestNow },
	}, c
}

func recordExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: orphanTestNS, Name: name}, &corev1.ConfigMap{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return true
}

func TestAsyncOrphanPrune(t *testing.T) {
	past := rfc(orphanTestNow.Add(-time.Minute))
	future := rfc(orphanTestNow.Add(10 * time.Minute))
	created := orphanTestNow.Add(-61 * time.Minute)

	live := &kaalmv1beta1.AgentChannel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "live"}}
	objs := []client.Object{
		live,
		// Orphans: the channel is gone.
		asyncRecord("kaalm-async-orphan-expired", "team", "gone", past, created),
		asyncRecord("kaalm-async-orphan-fresh", "team", "gone", future, orphanTestNow.Add(-50*time.Minute)),
		// A live channel's records belong to that channel's own prune.
		asyncRecord("kaalm-async-live-expired", "team", "live", past, created),
		// No parseable expiry: fall back to creationTimestamp, conservatively.
		asyncRecord("kaalm-async-bad-expiry-old", "team", "gone", "not-a-time", orphanTestNow.Add(-3*time.Hour)),
		asyncRecord("kaalm-async-bad-expiry-young", "team", "gone", "", orphanTestNow.Add(-90*time.Minute)),
		// No channel labels: nothing to look up, so the same fallback applies.
		asyncRecord("kaalm-async-unlabeled-old", "", "", "", orphanTestNow.Add(-3*time.Hour)),
		asyncRecord("kaalm-async-unlabeled-young", "", "", "", orphanTestNow.Add(-30*time.Minute)),
		asyncRecord("kaalm-async-unlabeled-expired", "", "", past, created),
		// Not an async record at all.
		asyncRecord("kaalm-budget-x", "team", "gone", past, orphanTestNow.Add(-3*time.Hour)),
	}
	other := asyncRecord("kaalm-async-other-ns", "team", "gone", past, created)
	other.Namespace = "elsewhere"
	objs = append(objs, other)

	p, c := newOrphanPruner(t, objs...)
	if err := p.pruneOnce(context.Background()); err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}

	want := map[string]bool{
		"kaalm-async-orphan-expired":    false,
		"kaalm-async-orphan-fresh":      true,
		"kaalm-async-live-expired":      true,
		"kaalm-async-bad-expiry-old":    false,
		"kaalm-async-bad-expiry-young":  true,
		"kaalm-async-unlabeled-old":     false,
		"kaalm-async-unlabeled-young":   true,
		"kaalm-async-unlabeled-expired": false,
		"kaalm-budget-x":                true,
	}
	for name, keep := range want {
		if got := recordExists(t, c, name); got != keep {
			t.Errorf("%s: exists=%v, want %v", name, got, keep)
		}
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "elsewhere", Name: "kaalm-async-other-ns"},
		&corev1.ConfigMap{}); err != nil {
		t.Errorf("record outside the operator namespace must be left alone: %v", err)
	}
}

// The runnable is leader-only and runs a pass at start, then on its ticker.
func TestAsyncOrphanPruner_Start(t *testing.T) {
	p, c := newOrphanPruner(t,
		asyncRecord("kaalm-async-orphan", "team", "gone", rfc(orphanTestNow.Add(-time.Minute)), orphanTestNow.Add(-2*time.Hour)))
	if !p.NeedLeaderElection() {
		t.Fatal("the orphan prune must run on the leader only")
	}
	p.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for recordExists(t, c, "kaalm-async-orphan") {
		if time.Now().After(deadline) {
			t.Fatal("the first pass must run at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v after cancel, want nil", err)
	}
}
