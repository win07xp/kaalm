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

package secretwatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestReaderServesSecretsAndCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "v1"))
	r := NewReader(New(ctx, cs))
	key := types.NamespacedName{Namespace: "team-a", Name: "hook"}

	var sec corev1.Secret
	if err := r.Get(ctx, key, &sec); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := string(sec.Data["token"]); got != "v1" {
		t.Fatalf("token = %q, want v1", got)
	}
	// The caller's copy is its own: a write to it never reaches the cache.
	sec.Data["token"] = []byte("mutated")
	var again corev1.Secret
	if err := r.Get(ctx, key, &again); err != nil {
		t.Fatal(err)
	}
	if got := string(again.Data["token"]); got != "v1" {
		t.Fatalf("cached token = %q after a caller write, want v1", got)
	}
}

func TestReaderRejectsOtherTypesAndList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReader(New(ctx, fake.NewSimpleClientset()))

	err := r.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "cm"}, &corev1.ConfigMap{})
	if err == nil || apierrors.IsNotFound(err) {
		t.Fatalf("Get(ConfigMap) = %v, want a type error", err)
	}
	if err := r.List(ctx, &corev1.SecretList{}); !errors.Is(err, errListUnsupported) {
		t.Fatalf("List = %v, want errListUnsupported", err)
	}
}

// The reasons CredentialsMissing, CallbackAuthMissing, and
// ImagePullSecretMissing key off apierrors.IsNotFound.
func TestReaderReportsAbsentSecretAsNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReader(New(ctx, fake.NewSimpleClientset()))

	err := r.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "gone"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("absent Secret: err = %v, want NotFound", err)
	}
}

// The controller retries Forbidden while a new Role reaches the authorizer,
// so a forbidden read must stay a Forbidden, not a timeout or NotFound.
func TestReaderReportsForbiddenAsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "x"))
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no grant"))
	})
	w := New(ctx, cs)
	w.SyncTimeout = 50 * time.Millisecond

	err := NewReader(w).Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "hook"}, &corev1.Secret{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("forbidden read: err = %v, want Forbidden", err)
	}
}

func TestReaderRepeatedReadsIssueOneGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "x"))
	var gets atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return false, nil, nil
	})
	r := NewReader(New(ctx, cs))
	for range 10 {
		if err := r.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "hook"}, &corev1.Secret{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("10 reads cost %d GETs, want 1 (the informer's initial read)", n)
	}
}
