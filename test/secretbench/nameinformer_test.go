//go:build secretbench

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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNameInformerReaderFollowsOneSecret(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	client, counter := countedClient(t)
	r := newNameInformerReader(ctx, client, 0)
	defer stopAndDrain(t, r, counter)

	created := createSecret(t, ns, "one", false)
	got, err := r.Get(ctx, ns, "one")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != created.ResourceVersion {
		t.Fatalf("Get returned version %s, want %s", got.ResourceVersion, created.ResourceVersion)
	}

	rv, err := updateSecret(ctx, adminClient, ns, "one")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the update to reach the reader", func() bool {
		sec, err := r.Get(ctx, ns, "one")
		return err == nil && sec.ResourceVersion == rv
	})

	if err := adminClient.CoreV1().Secrets(ns).Delete(ctx, "one", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the delete to reach the reader", func() bool {
		_, err := r.Get(ctx, ns, "one")
		return apierrors.IsNotFound(err)
	})

	if _, err := r.Get(ctx, ns, "later"); !apierrors.IsNotFound(err) {
		t.Fatalf("Get of an absent Secret = %v, want NotFound", err)
	}
	createSecret(t, ns, "later", false)
	eventually(t, 5*time.Second, "a created Secret to reach the reader", func() bool {
		_, err := r.Get(ctx, ns, "later")
		return err == nil
	})
}

func TestNameInformerReaderListsThenWatches(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	client, counter := countedClient(t)
	r := newNameInformerReader(ctx, client, 0)
	defer stopAndDrain(t, r, counter)

	createSecret(t, ns, "one", false)
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	settle(t, counter, 1)
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests = %+v, want List 1, Watch 1", got)
	}
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests after a cached read = %+v, want List 1, Watch 1", got)
	}
}

// Today's Watcher stops an informer whose list is refused as Forbidden and
// answers the refusal; C must too, or it serves its store after the grant is
// gone. An open watch outlives its grant, so the refusal shows once the
// 1 s watch ends and the relist is refused.
func TestNameInformerReaderStopsOnRevokedGrant(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	createSecret(t, ns, "one", false)
	createSecret(t, ns, "two", false)
	userCfg, regrant := grantedUser(t, ns, "one", "two")
	_, client, counter := countedClientFor(t, userCfg)
	r := newNameInformerReader(ctx, client, time.Second)
	defer stopAndDrain(t, r, counter)

	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("Get as the granted user: %v", err)
	}
	regrant("two")
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := r.Get(ctx, ns, "one")
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Get after the grant was removed = %v, want Forbidden within 15s", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := r.Get(ctx, ns, "two"); err != nil {
		t.Fatalf("Get of a still-granted Secret: %v", err)
	}
}
