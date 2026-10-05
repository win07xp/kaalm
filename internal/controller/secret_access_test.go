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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "team-a",
				Labels:      map[string]string{kaalmv1beta1.LabelChannelCredential: kaalmv1beta1.AnnotationTrue},
				Annotations: map[string]string{kaalmv1beta1.AnnotationCallbackHosts: "example.com"},
			},
			Data: map[string][]byte{"token": []byte("t")},
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
		if reason, msg, _ := r.validateSecrets(ctx, ch); reason != "" {
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
	if reason, _, _ := r.validateSecrets(ctx, ch); reason != kaalmv1beta1.ReasonCredentialsMissing {
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

// envSecretRefs keeps only secretKeyRef names, once each, sorted, and leaves
// out an empty name.
func TestEnvSecretRefs(t *testing.T) {
	ref := func(name string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "k",
		}}
	}
	env := []corev1.EnvVar{
		{Name: "PLAIN", Value: "v"},
		{Name: "B", ValueFrom: ref("zz")},
		{Name: "CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "k",
		}}},
		{Name: "FIELD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "RES", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
		{Name: "A", ValueFrom: ref("aa")},
		{Name: "A2", ValueFrom: ref("aa")},
		{Name: "EMPTY", ValueFrom: ref("")},
	}
	got := envSecretRefs(env)
	want := []corev1.LocalObjectReference{{Name: "aa"}, {Name: "zz"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("envSecretRefs = %+v, want %+v", got, want)
	}
	if got := envSecretRefs(nil); len(got) != 0 {
		t.Errorf("envSecretRefs(nil) = %+v, want none", got)
	}
}

// errReader answers every read with err.
type errReader struct {
	client.Reader
	err error
}

func (e errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}

// Rule 48: a workload env may name only a Secret labeled for workload use.
// Missing and unlabeled Secrets get one reason and one message, and the
// message never names a key or a value.
func TestCheckEnvSecrets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secret := func(name string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels},
			Data:       map[string][]byte{"hidden-key": []byte("hidden-value")},
		}
	}
	optedIn := map[string]string{kaalmv1beta1.LabelWorkloadSecret: kaalmv1beta1.AnnotationTrue}
	cs := kubefake.NewSimpleClientset(
		secret("labeled", optedIn),
		secret("unlabeled", nil),
		secret("wrong-value", map[string]string{kaalmv1beta1.LabelWorkloadSecret: "True"}),
		secret("channel-only", map[string]string{kaalmv1beta1.LabelChannelCredential: kaalmv1beta1.AnnotationTrue}),
	)
	reader := secretwatch.NewReader(secretwatch.New(ctx, cs))
	envRef := func(envName, secretName string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: envName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: "hidden-key", Optional: &optional,
		}}}
	}

	if reason, msg, err := checkEnvSecrets(ctx, reader, "team-a", []corev1.EnvVar{
		{Name: "PLAIN", Value: "v"}, envRef("TOKEN", "labeled", false),
	}); reason != "" || err != nil {
		t.Fatalf("labeled Secret: reason=%q msg=%q err=%v, want a pass", reason, msg, err)
	}

	for _, tc := range []struct {
		name, secret string
		optional     bool
	}{
		{"unlabeled", "unlabeled", false},
		{"wrong value", "wrong-value", false},
		{"channel label only", "channel-only", false},
		{"missing", "absent", false},
		{"optional but missing", "absent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg, err := checkEnvSecrets(ctx, reader, "team-a",
				[]corev1.EnvVar{envRef("TOKEN", tc.secret, tc.optional)})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if reason != kaalmv1beta1.ReasonSecretNotOptedIn {
				t.Fatalf("reason = %q, want %q", reason, kaalmv1beta1.ReasonSecretNotOptedIn)
			}
			if want := envSecretNotUsableMessage("TOKEN", tc.secret); msg != want {
				t.Errorf("msg = %q, want %q", msg, want)
			}
			for _, part := range []string{"TOKEN", tc.secret, kaalmv1beta1.LabelWorkloadSecret} {
				if !strings.Contains(msg, part) {
					t.Errorf("msg %q lacks %q", msg, part)
				}
			}
			for _, leak := range []string{"hidden-key", "hidden-value"} {
				if strings.Contains(msg, leak) {
					t.Errorf("msg %q names %q", msg, leak)
				}
			}
		})
	}

	// Missing and unlabeled read the same apart from the names in them.
	_, missing, _ := checkEnvSecrets(ctx, reader, "team-a", []corev1.EnvVar{envRef("X", "s", false)})
	if missing != envSecretNotUsableMessage("X", "s") {
		t.Errorf("missing-Secret message = %q", missing)
	}

	reason, msg, err := checkEnvSecrets(ctx, reader, "team-a", []corev1.EnvVar{envRef("EMPTY", "", false)})
	if err != nil || reason != kaalmv1beta1.ReasonInvalidReference || !strings.Contains(msg, "EMPTY") {
		t.Errorf("empty name: reason=%q msg=%q err=%v, want InvalidReference", reason, msg, err)
	}

	// The first failing entry in spec order is reported.
	_, msg, _ = checkEnvSecrets(ctx, reader, "team-a", []corev1.EnvVar{
		envRef("OK", "labeled", false), envRef("FIRST", "unlabeled", false), envRef("SECOND", "absent", false),
	})
	if msg != envSecretNotUsableMessage("FIRST", "unlabeled") {
		t.Errorf("msg = %q, want the first failing entry", msg)
	}

	// Any other read error is returned as an error, not a status reason.
	boom := errors.New("boom")
	reason, _, err = checkEnvSecrets(ctx, errReader{err: boom}, "team-a", []corev1.EnvVar{envRef("T", "labeled", false)})
	if !errors.Is(err, boom) || reason != "" {
		t.Errorf("read error: reason=%q err=%v, want the error", reason, err)
	}
}

// Several entries naming one Secret cost one read.
func TestCheckEnvSecrets_ReadsEachSecretOnce(t *testing.T) {
	env := []corev1.EnvVar{}
	for _, n := range []string{"A", "B", "C"} {
		env = append(env, corev1.EnvVar{Name: n, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "shared"}, Key: n,
		}}})
	}
	labeled := &labeledReader{}
	if reason, _, err := checkEnvSecrets(context.Background(), labeled, "team-a", env); reason != "" || err != nil {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if labeled.calls != 1 {
		t.Errorf("reads = %d, want 1", labeled.calls)
	}
}

// labeledReader answers every Secret read with an opted-in Secret.
type labeledReader struct {
	client.Reader
	calls int
}

func (l *labeledReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	l.calls++
	obj.SetLabels(map[string]string{kaalmv1beta1.LabelWorkloadSecret: kaalmv1beta1.AnnotationTrue})
	return nil
}

// An update or delete of the Secret-access Role or RoleBinding that the API
// server refuses comes back as a rejected write; a delete of an object
// already gone is no error.
func TestEnsureControllerSecretAccess_RejectedWrite(t *testing.T) {
	scheme := testScheme(t)
	owner := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: "default", UID: "agent-uid"}}
	const roleName = "kaalm-agent-sa-pull"
	newClient := func(notFoundOnDelete bool) client.Client {
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: "default"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{"old"}, Verbs: []string{"get", "watch"},
			}},
		}
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: "default"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		}
		for _, obj := range []client.Object{role, rb} {
			if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
				t.Fatal(err)
			}
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(role, rb).
			WithInterceptorFuncs(rejectingWrites(notFoundOnDelete)).Build()
	}

	err := ensureControllerSecretAccess(ctxT(), newClient(false), scheme, owner, roleName, "kaalm-system",
		[]corev1.LocalObjectReference{{Name: "new"}})
	expectWriteRejected(t, err, "updating", "Role")
	err = ensureControllerSecretAccess(ctxT(), newClient(false), scheme, owner, roleName, "kaalm-system", nil)
	expectWriteRejected(t, err, "deleting", "RoleBinding")
	if err := ensureControllerSecretAccess(ctxT(), newClient(true), scheme, owner, roleName, "kaalm-system",
		nil); err != nil {
		t.Errorf("delete of objects already gone: err = %v, want nil", err)
	}
}

// The Role delete is reported too, once its RoleBinding is gone.
func TestEnsureControllerSecretAccess_RoleDeleteRejected(t *testing.T) {
	scheme := testScheme(t)
	owner := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: "default", UID: "agent-uid"}}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "kaalm-agent-sa-pull", Namespace: "default"}}
	if err := controllerutil.SetControllerReference(owner, role, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(role).
		WithInterceptorFuncs(rejectingWrites(false)).Build()
	err := ensureControllerSecretAccess(ctxT(), c, scheme, owner, role.Name, "kaalm-system", nil)
	expectWriteRejected(t, err, "deleting", "Role")
}
