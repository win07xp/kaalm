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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// scopeFakeChannel is channel "gate-ch" with its finalizer, bearer auth on
// the labeled Secret gate-labeled and HMAC on the unlabeled gate-unlabeled.
func scopeFakeChannel(path string) *kaalmv1beta1.AgentChannel {
	return &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gate-ch", Namespace: "default", UID: "gate-ch-uid",
			Finalizers: []string{kaalmv1beta1.ChannelFinalizer},
		},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "gate-agent"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: path,
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "gate-labeled", Key: "token"},
					HMAC: &kaalmv1beta1.ChannelHMAC{
						Header:    "X-Signature",
						SecretRef: kaalmv1beta1.SecretKeyReference{Name: "gate-unlabeled", Key: "token"},
					},
				},
			},
		},
	}
}

// preScopeObjects are the Secrets plus the credential Role and controller
// RoleBinding an older release left: the Role grants every referenced
// Secret, labeled or not.
func preScopeObjects(t *testing.T, ch *kaalmv1beta1.AgentChannel) []client.Object {
	t.Helper()
	labeled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gate-labeled", Namespace: "default", Labels: channelCredentialLabels()},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	unlabeled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gate-unlabeled", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: channelRoleName(ch.Name), Namespace: "default"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{"gate-labeled", "gate-unlabeled"}, Verbs: []string{"get", "watch"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: channelControllerCredsBindingName(ch.Name), Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: channelRoleName(ch.Name)},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: controllerServiceAccount, Namespace: testSystemNamespace,
		}},
	}
	scheme := testScheme(t)
	for _, obj := range []client.Object{role, rb} {
		if err := controllerutil.SetControllerReference(ch, obj, scheme); err != nil {
			t.Fatal(err)
		}
	}
	return []client.Object{labeled, unlabeled, role, rb}
}

// scopeReconcile runs one channel pass on a fake client holding objs.
func scopeReconcile(t *testing.T, ch *kaalmv1beta1.AgentChannel, objs ...client.Object) client.Client {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append(objs, ch)...).WithStatusSubresource(ch).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	if _, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

// expectFakeReady checks the channel's Ready condition is False with reason.
func expectFakeReady(t *testing.T, c client.Client, ch *kaalmv1beta1.AgentChannel, reason string) {
	t.Helper()
	var got kaalmv1beta1.AgentChannel
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ch), &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reason {
		t.Fatalf("Ready = %+v, want False/%s", cond, reason)
	}
}

// fakeCredsRoleNames returns the Secret names the credential Role grants.
func fakeCredsRoleNames(t *testing.T, c client.Client, ch *kaalmv1beta1.AgentChannel) []string {
	t.Helper()
	var role rbacv1.Role
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: channelRoleName(ch.Name)}, &role); err != nil {
		t.Fatalf("credential Role: %v", err)
	}
	var names []string
	for _, rule := range role.Rules {
		names = append(names, rule.ResourceNames...)
	}
	return names
}

// A channel stopped by the Agent, service, or path check keeps no credential
// Role wider than its labeled Secrets, and loses the old controller binding.
func TestChannel_GateScopesCredentialRole(t *testing.T) {
	agent := func(mutate func(*kaalmv1beta1.Agent)) *kaalmv1beta1.Agent {
		ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "gate-agent", Namespace: "default"}}
		if mutate != nil {
			mutate(ag)
		}
		return ag
	}
	winner := scopeFakeChannel("/channels/default/gate-ch")
	winner.Name, winner.UID = "a-winner", "a-winner-uid"
	cases := []struct {
		name   string
		path   string
		extra  []client.Object
		reason string
	}{
		{"AgentNotFound", "/channels/default/gate-ch", nil, kaalmv1beta1.ReasonAgentNotFound},
		{"AgentServiceDisabled", "/channels/default/gate-ch", []client.Object{agent(func(ag *kaalmv1beta1.Agent) {
			ag.Spec.Service = &kaalmv1beta1.AgentService{Enabled: false}
		})}, kaalmv1beta1.ReasonAgentServiceDisabled},
		{"InvalidPath", "/channels/other/x", []client.Object{agent(nil)}, kaalmv1beta1.ReasonInvalidPath},
		{"PathConflict", "/channels/default/gate-ch", []client.Object{agent(nil), winner},
			kaalmv1beta1.ReasonPathConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := scopeFakeChannel(tc.path)
			c := scopeReconcile(t, ch, append(preScopeObjects(t, ch), tc.extra...)...)
			expectFakeReady(t, c, ch, tc.reason)
			if got := fakeCredsRoleNames(t, c, ch); strings.Join(got, ",") != "gate-labeled" {
				t.Errorf("credential Role grants %v, want [gate-labeled]", got)
			}
			err := c.Get(context.Background(), types.NamespacedName{
				Namespace: "default", Name: channelControllerCredsBindingName(ch.Name)}, &rbacv1.RoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("controller creds RoleBinding: err = %v, want NotFound", err)
			}
			if err := c.Get(context.Background(), types.NamespacedName{
				Namespace: "default", Name: channelRoleName(ch.Name) + "-gateway"}, &rbacv1.RoleBinding{}); err != nil {
				t.Errorf("gateway RoleBinding: %v", err)
			}
		})
	}
}

// Once a gated channel's Roles have converged, a later gated pass makes no
// API write.
func TestChannel_GatedPassWritesNothing(t *testing.T) {
	ch := gateFakeChannel()
	ch.UID = "ch-gate-uid"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-gate-secret", Namespace: "default", Labels: channelCredentialLabels()},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	var writes int
	count := func() { writes++ }
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(ch, sec).WithStatusSubresource(ch).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				count()
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				count()
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				count()
				return c.Patch(ctx, obj, p, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				count()
				return c.Delete(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				count()
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch,
				opts ...client.SubResourcePatchOption) error {
				count()
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	writes = 0
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if writes != 0 {
		t.Errorf("a converged gated pass made %d writes, want 0", writes)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, time.Minute)
	}
}

// Without its check Role the controller cannot read a label, so no Secret
// counts as opted in: the credential Role is emptied, not left granting
// every referenced Secret.
func TestChannel_CheckRoleConflictEmptiesCredentialRole(t *testing.T) {
	cases := []struct {
		name   string
		extra  []client.Object
		reason string
	}{
		{"valid Agent", []client.Object{
			&kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "gate-agent", Namespace: "default"}},
		}, kaalmv1beta1.ReasonChildConflict},
		{"no Agent", nil, kaalmv1beta1.ReasonAgentNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := scopeFakeChannel("/channels/default/gate-ch")
			foreign := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: channelCheckRoleName(ch.Name), Namespace: "default"}}
			objs := append(preScopeObjects(t, ch), foreign)
			c := scopeReconcile(t, ch, append(objs, tc.extra...)...)
			expectFakeReady(t, c, ch, tc.reason)
			if got := fakeCredsRoleNames(t, c, ch); len(got) != 0 {
				t.Errorf("credential Role grants %v, want no Secret", got)
			}
		})
	}
}

// roleWriteFailure makes the fake client fail one write, named by verb
// ("create" or "update") and object name, with err.
func roleWriteFailure(verb, name string, err error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if verb == "create" && obj.GetName() == name {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if verb == "update" && obj.GetName() == name {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// A per-channel Role or RoleBinding write the API server refuses is cluster
// policy, not a wrong reference: Ready=False ChildWriteRejected, one Warning
// event, and the conflict cadence, since a policy change raises no event.
func TestChannel_RoleWriteRejectedIsChildWriteRejected(t *testing.T) {
	forbidden := apierrors.NewForbidden(rbacv1.Resource("roles"), "x", nil)
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "gate-agent", Namespace: "default"}}
	cases := []struct {
		name, verb, object string
		preScope           bool
	}{
		{"check Role create", "create", "kaalm-channel-gate-ch-check", true},
		{"credential Role create", "create", "kaalm-channel-gate-ch-creds", false},
		{"gateway RoleBinding create", "create", "kaalm-channel-gate-ch-creds-gateway", false},
		{"credential Role update", "update", "kaalm-channel-gate-ch-creds", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := scopeFakeChannel("/channels/default/gate-ch")
			objs := preScopeObjects(t, ch)
			if !tc.preScope {
				objs = objs[:2] // the Secrets only
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(append(objs, agent, ch)...).WithStatusSubresource(ch).
				WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).
				WithInterceptorFuncs(roleWriteFailure(tc.verb, tc.object, forbidden)).Build()
			rec := record.NewFakeRecorder(10)
			r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace, Recorder: rec}
			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			expectFakeReady(t, c, ch, kaalmv1beta1.ReasonChildWriteRejected)
			if res.RequeueAfter != gateRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, gateRequeue)
			}
			close(rec.Events)
			var events []string
			for e := range rec.Events {
				events = append(events, e)
			}
			if len(events) != 1 || !strings.Contains(events[0], "Warning "+kaalmv1beta1.ReasonChildWriteRejected) {
				t.Errorf("events = %q, want one ChildWriteRejected Warning", events)
			}
			if tc.object == channelCheckRoleName(ch.Name) {
				if got := fakeCredsRoleNames(t, c, ch); len(got) != 0 {
					t.Errorf("credential Role grants %v, want no Secret", got)
				}
			}
		})
	}
}

// A transient Role write failure is a reconcile error retried with backoff,
// not a status reason.
func TestChannel_RoleWriteTransientErrorRetries(t *testing.T) {
	ch := scopeFakeChannel("/channels/default/gate-ch")
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "gate-agent", Namespace: "default"}}
	unavailable := apierrors.NewServiceUnavailable("etcd is down")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append(preScopeObjects(t, ch)[:2], agent, ch)...).WithStatusSubresource(ch).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).
		WithInterceptorFuncs(roleWriteFailure("create", channelRoleName(ch.Name), unavailable)).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)})
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("reconcile error = %v, want ServiceUnavailable", err)
	}
	var got kaalmv1beta1.AgentChannel
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ch), &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady); cond != nil &&
		cond.Reason == kaalmv1beta1.ReasonInvalidReference {
		t.Errorf("Ready = %+v, want no InvalidReference", cond)
	}
}
