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

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestLabelInformerReaderServesLabeledSecretsOnly(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	client, counter := countedClient(t)
	r := newLabelInformerReader(ctx, client)
	defer stopAndDrain(t, r, counter)

	createSecret(t, ns, "labeled", true)
	createSecret(t, ns, "plain", false)
	if _, err := r.Get(ctx, ns, "labeled"); err != nil {
		t.Fatalf("Get of the labeled Secret: %v", err)
	}
	if _, err := r.Get(ctx, ns, "plain"); !apierrors.IsNotFound(err) {
		t.Fatalf("Get of the unlabeled Secret = %v, want NotFound", err)
	}

	setLabel := func(on bool) {
		t.Helper()
		sec, err := adminClient.CoreV1().Secrets(ns).Get(ctx, "labeled", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if on {
			sec.Labels = map[string]string{kaalmv1beta1.LabelChannelCredential: "true"}
		} else {
			sec.Labels = nil
		}
		if _, err := adminClient.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	setLabel(false)
	eventually(t, 5*time.Second, "the unlabeled Secret to leave the reader", func() bool {
		_, err := r.Get(ctx, ns, "labeled")
		return apierrors.IsNotFound(err)
	})
	setLabel(true)
	eventually(t, 5*time.Second, "the relabeled Secret to return", func() bool {
		_, err := r.Get(ctx, ns, "labeled")
		return err == nil
	})
}

func TestLabelInformerReaderOneWatchPerNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	first, second := testNamespace(t), testNamespace(t)
	client, counter := countedClient(t)
	r := newLabelInformerReader(ctx, client)
	defer stopAndDrain(t, r, counter)

	createSecret(t, first, "a", true)
	createSecret(t, first, "b", true)
	createSecret(t, second, "c", true)
	for _, name := range []string{"a", "b"} {
		if _, err := r.Get(ctx, first, name); err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
	}
	settle(t, counter, 1)
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests for two Secrets in one namespace = %+v, want List 1, Watch 1", got)
	}
	if _, err := r.Get(ctx, second, "c"); err != nil {
		t.Fatalf("Get c: %v", err)
	}
	settle(t, counter, 2)
	if got := counter.snapshot(); got != (requestCounts{List: 2, Watch: 2}) {
		t.Fatalf("requests after a second namespace = %+v, want List 2, Watch 2", got)
	}
}
