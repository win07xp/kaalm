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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/secretwatch"
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

// countSecretGets counts the GETs the clientset answers for Secrets.
func countSecretGets(cs *kubefake.Clientset) *atomic.Int32 {
	var n atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

// The channel reconciler re-validates every channel every minute. Through the
// watcher those passes are cache hits: the inbound and callback Secrets each
// cost one GET, however many passes run.
func TestValidateSecrets_RepeatedPassesReadFromTheWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Data:       map[string][]byte{"token": []byte("t")},
		}
	}
	cs := kubefake.NewSimpleClientset(secret("in"), secret("cb"))
	gets := countSecretGets(cs)
	r := &AgentChannelReconciler{
		Client:       fake.NewClientBuilder().Build(),
		SecretReader: secretwatch.NewReader(secretwatch.New(ctx, cs)),
	}
	cbURL := "https://example.com/hook"
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{
			Auth: kaalmv1beta1.ChannelAuth{
				Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "in", Key: "token"},
			},
			CallbackURL: &cbURL,
			CallbackAuth: &kaalmv1beta1.ChannelAuth{
				Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "cb", Key: "token"},
			},
		}},
	}
	for pass := 0; pass < 20; pass++ {
		if reason, msg := r.validateSecrets(ctx, ch); reason != "" {
			t.Fatalf("pass %d: %s: %s", pass, reason, msg)
		}
	}
	if n := gets.Load(); n != 2 {
		t.Fatalf("20 validation passes cost %d Secret GETs, want 2", n)
	}
}

// A missing Secret still reports CredentialsMissing through the watcher, with
// the message getSecretLive's callers build from a NotFound.
func TestValidateSecrets_MissingSecretThroughTheWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &AgentChannelReconciler{
		Client:       fake.NewClientBuilder().Build(),
		SecretReader: secretwatch.NewReader(secretwatch.New(ctx, kubefake.NewSimpleClientset())),
	}
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{
			Auth: kaalmv1beta1.ChannelAuth{
				Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "absent", Key: "token"},
			},
		}},
	}
	if reason, _ := r.validateSecrets(ctx, ch); reason != kaalmv1beta1.ReasonCredentialsMissing {
		t.Fatalf("reason = %q, want %q", reason, kaalmv1beta1.ReasonCredentialsMissing)
	}
}

// A Role created a moment ago answers Forbidden until the authorizer sees it.
// Through the watcher that is still a Forbidden, so getSecretLive retries it
// and the read succeeds once the grant lands.
func TestGetSecretLive_RetriesForbiddenThroughTheWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := kubefake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-a"},
	})
	var calls atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) <= 2 {
			return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), "creds", errors.New("not yet"))
		}
		return false, nil, nil
	})
	w := secretwatch.New(ctx, cs)
	w.SyncTimeout = 50 * time.Millisecond
	key := types.NamespacedName{Namespace: "team-a", Name: "creds"}
	if err := getSecretLive(ctx, secretwatch.NewReader(w), key, &corev1.Secret{}); err != nil {
		t.Fatalf("read after the grant lands = %v, want nil", err)
	}
}
