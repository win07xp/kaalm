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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// forbiddenThenOK answers Forbidden for the first n reads, as the apiserver
// does while a Role created a moment ago reaches its authorizer.
type forbiddenThenOK struct {
	client.Reader
	forbidden int
	calls     int
}

func (f *forbiddenThenOK) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	f.calls++
	if f.calls <= f.forbidden {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, nil)
	}
	return nil
}

func TestGetSecretLive_RetriesForbidden(t *testing.T) {
	key := types.NamespacedName{Namespace: "team-a", Name: "creds"}

	settles := &forbiddenThenOK{forbidden: 2}
	if err := getSecretLive(context.Background(), settles, key, &corev1.Secret{}); err != nil {
		t.Errorf("a read that settles on the third attempt = %v, want nil", err)
	}
	if settles.calls != 3 {
		t.Errorf("calls = %d, want 3", settles.calls)
	}

	never := &forbiddenThenOK{forbidden: secretReadAttempts + 1}
	if err := getSecretLive(context.Background(), never, key, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Errorf("a read that stays Forbidden = %v, want Forbidden", err)
	}
	if never.calls != secretReadAttempts {
		t.Errorf("calls = %d, want %d", never.calls, secretReadAttempts)
	}
}

func TestLiveSecretReader_FallsBack(t *testing.T) {
	fallback := &forbiddenThenOK{}
	if liveSecretReader(nil, fallback) != client.Reader(fallback) {
		t.Error("a nil reader must fall back to the embedded client")
	}
	live := &forbiddenThenOK{}
	if liveSecretReader(live, fallback) != client.Reader(live) {
		t.Error("a set reader must win")
	}
}
