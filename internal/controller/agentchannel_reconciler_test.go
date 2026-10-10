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
	"fmt"
	"net"
	"sort"
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
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/callbackpolicy"
)

func mkChannel(t *testing.T, name, agentName, path string, mutate func(*kaalmv1beta1.AgentChannel)) {
	t.Helper()
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: agentName},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: path,
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: name + "-secret", Key: "token"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(ch)
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
}

// mkChannelSecret creates a channel Secret that opts in to channel use
// (rule 45).
func mkChannelSecret(t *testing.T, name string) {
	t.Helper()
	mkDefaultSecret(t, name, channelCredentialLabels(), nil, map[string][]byte{"token": []byte("hook-token")})
}

// mkUnlabeledChannelSecret creates a Secret with a token key but without the
// rule 45 opt-in label.
func mkUnlabeledChannelSecret(t *testing.T, name string) {
	t.Helper()
	mkDefaultSecret(t, name, nil, nil, map[string][]byte{"token": []byte("hook-token")})
}

// mkCallbackSecret creates an opted-in Secret whose kaalm.io/callback-hosts
// annotation lists hosts (rule 46).
func mkCallbackSecret(t *testing.T, name, hosts string, data map[string][]byte) {
	t.Helper()
	mkDefaultSecret(t, name, channelCredentialLabels(),
		map[string]string{kaalmv1beta1.AnnotationCallbackHosts: hosts}, data)
}

func channelCredentialLabels() map[string]string {
	return map[string]string{kaalmv1beta1.LabelChannelCredential: kaalmv1beta1.AnnotationTrue}
}

func mkDefaultSecret(t *testing.T, name string, labels, annotations map[string]string, data map[string][]byte) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, Annotations: annotations},
		Data:       data,
	}
	if err := testClient.Create(ctxT(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

// editSecret applies mutate to a Secret in default, retrying on conflict.
func editSecret(t *testing.T, name string, mutate func(*corev1.Secret)) {
	t.Helper()
	eventually(t, func() error {
		var sec corev1.Secret
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &sec); err != nil {
			return err
		}
		mutate(&sec)
		return testClient.Update(ctxT(), &sec)
	})
}

// getRole reads a Role in default.
func getRole(name string) (*rbacv1.Role, error) {
	var role rbacv1.Role
	err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &role)
	return &role, err
}

// credsRoleNames returns the Secret names the gateway-facing -creds Role
// grants, or nil when it has no rules.
func credsRoleNames(channel string) ([]string, error) {
	role, err := getRole("kaalm-channel-" + channel + "-creds")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range role.Rules {
		if len(r.ResourceNames) == 0 {
			return nil, errString("a -creds rule with no resourceNames grants every Secret")
		}
		names = append(names, r.ResourceNames...)
	}
	sort.Strings(names)
	return names, nil
}

func expectChannelReady(t *testing.T, name string, want metav1.ConditionStatus, reason string) {
	t.Helper()
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
			return err
		}
		c := condition(ch.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil {
			return errString("no Ready condition yet")
		}
		if c.Status != want {
			return errString("Ready=" + string(c.Status) + " want " + string(want) + " reason=" + c.Reason)
		}
		if reason != "" && c.Reason != reason {
			return errString("reason=" + c.Reason + " want " + reason)
		}
		return nil
	})
}

func TestChannel_ValidBecomesReady(t *testing.T) {
	mkWorkloadClass(t, "chc-ok", nil)
	mkWorkloadAgent(t, "ch-agent-ok", "chc-ok", nil)
	mkChannelSecret(t, "ch-ok-secret")
	mkChannel(t, "ch-ok", "ch-agent-ok", "/channels/default/ch-ok", nil)

	expectChannelReady(t, "ch-ok", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	// The scoped Role exists with exactly the auth Secret, get+watch only.
	var role rbacv1.Role
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-channel-ch-ok-creds"}, &role); err != nil {
		t.Fatalf("credential Role missing: %v", err)
	}
	rule := role.Rules[0]
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "ch-ok-secret" {
		t.Errorf("Role not scoped to the auth Secret: %v", rule.ResourceNames)
	}
	for _, v := range rule.Verbs {
		if v == "list" || v == "create" || v == "delete" {
			t.Errorf("Role must grant get/watch only, found %q", v)
		}
	}
	var rb rbacv1.RoleBinding
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-channel-ch-ok-creds-gateway"}, &rb); err != nil {
		t.Errorf("gateway RoleBinding missing: %v", err)
	}
	// The controller is not bound to the credential Role: the check Role
	// already grants it every name the credential Role lists.
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{
		Namespace: "default", Name: channelControllerCredsBindingName("ch-ok"),
	}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("controller RoleBinding to the credential Role: got err=%v, want NotFound", err)
	}
	// The controller-only check Role lists the Secret too (rule 45 reads its
	// label), bound to the controller alone.
	assertCheckRole(t, "ch-ok", []string{"ch-ok-secret"})
	// Phase reduces from the Agent (Pending and transients are Active).
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-ok"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != kaalmv1beta1.ChannelActive {
			return errString("phase=" + string(ch.Status.Phase))
		}
		return nil
	})
}

func TestChannel_AgentNotFound(t *testing.T) {
	mkChannelSecret(t, "ch-noagent-secret")
	mkChannel(t, "ch-noagent", "no-such-agent", "/channels/default/ch-noagent", nil)
	expectChannelReady(t, "ch-noagent", metav1.ConditionFalse, kaalmv1beta1.ReasonAgentNotFound)
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-noagent"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != kaalmv1beta1.ChannelFailed {
			return errString("phase=" + string(ch.Status.Phase) + " want Failed")
		}
		return nil
	})
}

func TestChannel_ServiceDisabled(t *testing.T) {
	mkWorkloadClass(t, "chc-svc", nil)
	mkWorkloadAgent(t, "ch-agent-svc", "chc-svc", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Service = &kaalmv1beta1.AgentService{Enabled: false}
	})
	mkChannelSecret(t, "ch-svc-secret")
	mkChannel(t, "ch-svc", "ch-agent-svc", "/channels/default/ch-svc", nil)
	expectChannelReady(t, "ch-svc", metav1.ConditionFalse, kaalmv1beta1.ReasonAgentServiceDisabled)
}

func TestChannel_InvalidPathPrefix(t *testing.T) {
	mkWorkloadClass(t, "chc-path", nil)
	mkWorkloadAgent(t, "ch-agent-path", "chc-path", nil)
	mkChannelSecret(t, "ch-path-secret")
	// Wrong namespace segment: rule 15.
	mkChannel(t, "ch-path", "ch-agent-path", "/channels/other-ns/ch-path", nil)
	expectChannelReady(t, "ch-path", metav1.ConditionFalse, kaalmv1beta1.ReasonInvalidPath)
}

func TestChannel_PathConflictNewerLoses(t *testing.T) {
	mkWorkloadClass(t, "chc-conf", nil)
	mkWorkloadAgent(t, "ch-agent-conf", "chc-conf", nil)
	mkChannelSecret(t, "ch-conf-a-secret")
	mkChannelSecret(t, "ch-conf-b-secret")
	mkChannel(t, "ch-conf-a", "ch-agent-conf", "/channels/default/shared-path", nil)
	expectChannelReady(t, "ch-conf-a", metav1.ConditionTrue, "")
	// creationTimestamp has 1-second resolution, so the sleep normally makes B
	// the newer channel and the test exercises "newer loses". The apiserver
	// stamps it from the wall clock, though, and a backward clock jump inside
	// the sleep can reverse the order. expectPathConflict therefore
	// picks the loser from the stored timestamps, not from creation order.
	time.Sleep(1100 * time.Millisecond)
	mkChannel(t, "ch-conf-b", "ch-agent-conf", "/channels/default/shared-path", nil)

	expectPathConflict(t, "ch-conf-a", "ch-conf-b")
}

// pathConflictLoser applies rule 15 as the book states it: of two channels on
// one path, the newer by creationTimestamp gets PathConflict, and when the
// timestamps are equal the one later by name does.
func pathConflictLoser(aName string, aCreated time.Time, bName string, bCreated time.Time) (loser, winner string) {
	switch {
	case aCreated.After(bCreated):
		return aName, bName
	case bCreated.After(aCreated):
		return bName, aName
	case aName > bName:
		return aName, bName
	default:
		return bName, aName
	}
}

// expectPathConflict reads the stored creationTimestamps of two channels on
// the same path, works out the loser from rule 15, and expects the loser to
// be Ready=False, reason=PathConflict and the winner to stay Ready=True. It
// returns the loser and the winner. Nothing re-runs the channels here: when
// the loser is the channel that was Ready first (a timestamp tie it loses by
// name, or a clock that went back), only the reconciler's re-enqueue of the
// channels sharing a path makes it re-check and see that it lost.
func expectPathConflict(t *testing.T, a, b string) (loser, winner string) {
	t.Helper()
	created := func(name string) time.Time {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
			t.Fatalf("read channel %s: %v", name, err)
		}
		return ch.CreationTimestamp.Time
	}
	loser, winner = pathConflictLoser(a, created(a), b, created(b))
	expectChannelReady(t, loser, metav1.ConditionFalse, kaalmv1beta1.ReasonPathConflict)
	expectChannelReady(t, winner, metav1.ConditionTrue, "")
	return loser, winner
}

// setChannelPath moves a webhook channel to another path.
func setChannelPath(t *testing.T, name, path string) {
	t.Helper()
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
			return err
		}
		ch.Spec.Webhook.Path = path
		return testClient.Update(ctxT(), &ch)
	})
}

// TestChannel_PathConflictExistingChannelLoses: a channel that moves onto a
// path held by a newer Ready channel wins it, and the newer channel turns
// PathConflict at once, not on its one-minute requeue. Nothing touches
// the Agent: only the other channel's change re-runs the loser.
func TestChannel_PathConflictExistingChannelLoses(t *testing.T) {
	mkWorkloadClass(t, "chc-pcx", nil)
	mkWorkloadAgent(t, "ch-agent-pcx", "chc-pcx", nil)
	mkChannelSecret(t, "ch-pcx-a-secret")
	mkChannelSecret(t, "ch-pcx-z-secret")
	// a is created first and sorts first by name, so it is the older channel
	// or wins the tie.
	mkChannel(t, "ch-pcx-a", "ch-agent-pcx", "/channels/default/pcx-other", nil)
	mkChannel(t, "ch-pcx-z", "ch-agent-pcx", "/channels/default/pcx-shared", nil)
	expectChannelReady(t, "ch-pcx-a", metav1.ConditionTrue, "")
	expectChannelReady(t, "ch-pcx-z", metav1.ConditionTrue, "")

	setChannelPath(t, "ch-pcx-a", "/channels/default/pcx-shared")
	expectPathConflict(t, "ch-pcx-a", "ch-pcx-z")
}

// TestChannel_PathConflictLoserWinsWhenWinnerLeaves: when the winner moves
// to another path or is deleted, the loser becomes Ready at once.
func TestChannel_PathConflictLoserWinsWhenWinnerLeaves(t *testing.T) {
	mkWorkloadClass(t, "chc-pcl", nil)
	mkWorkloadAgent(t, "ch-agent-pcl", "chc-pcl", nil)
	for _, n := range []string{"ch-pcl-a", "ch-pcl-b", "ch-pcl-c"} {
		mkChannelSecret(t, n+"-secret")
	}
	mkChannel(t, "ch-pcl-a", "ch-agent-pcl", "/channels/default/pcl-shared", nil)
	mkChannel(t, "ch-pcl-b", "ch-agent-pcl", "/channels/default/pcl-shared", nil)
	loser, winner := expectPathConflict(t, "ch-pcl-a", "ch-pcl-b")

	// The winner moves away: the old path's loser takes the path.
	setChannelPath(t, winner, "/channels/default/pcl-moved")
	expectChannelReady(t, loser, metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
	expectChannelReady(t, winner, metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	// A third channel on that path loses to the channel already there, which
	// is older, then takes the path once that channel is deleted.
	mkChannel(t, "ch-pcl-c", "ch-agent-pcl", "/channels/default/pcl-shared", nil)
	newLoser, newWinner := expectPathConflict(t, loser, "ch-pcl-c")
	var ch kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: newWinner}, &ch); err != nil {
		t.Fatal(err)
	}
	// Mark it disconnected so the finalizer releases without waiting for a
	// gateway.
	if ch.Annotations == nil {
		ch.Annotations = map[string]string{}
	}
	ch.Annotations[kaalmv1beta1.AnnotationChannelDisconnected] = kaalmv1beta1.AnnotationTrue
	if err := testClient.Update(ctxT(), &ch); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &ch); err != nil {
		t.Fatal(err)
	}
	expectChannelReady(t, newLoser, metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
}

// TestChannelPathSiblings: a channel's create, delete, or move to another
// path enqueues the other channels on each path it touched, in its own
// namespace; an update that keeps the path, such as a status write, enqueues
// nothing.
func TestChannelPathSiblings(t *testing.T) {
	onPath := func(ns, name, path string) *kaalmv1beta1.AgentChannel {
		return &kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{Path: path}},
		}
	}
	const p1, p2 = "/channels/team-a/one", "/channels/team-a/two"
	self := onPath("team-a", "self", p1)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).
		WithObjects(self,
			onPath("team-a", "one-x", p1), onPath("team-a", "one-y", p1),
			onPath("team-a", "two-x", p2),
			onPath("team-b", "one-other-ns", p1),
			onPath("team-a", "no-path", "")).
		Build()
	h := (&AgentChannelReconciler{Client: c}).pathSiblingHandler()
	ctx := context.Background()

	drain := func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) string {
		var names []string
		for q.Len() > 0 {
			req, _ := q.Get()
			names = append(names, req.Name)
			q.Done(req)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	newQueue := func() workqueue.TypedRateLimitingInterface[reconcile.Request] {
		return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	}

	moved := self.DeepCopy()
	moved.Spec.Webhook.Path = p2
	statusOnly := self.DeepCopy()
	statusOnly.Status.Phase = kaalmv1beta1.ChannelActive
	unset := self.DeepCopy()
	unset.Spec.Webhook.Path = ""

	cases := []struct {
		name string
		fire func(q workqueue.TypedRateLimitingInterface[reconcile.Request])
		want string
	}{
		{"create", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Create(ctx, event.CreateEvent{Object: self}, q)
		}, "one-x,one-y"},
		{"delete", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Delete(ctx, event.DeleteEvent{Object: self}, q)
		}, "one-x,one-y"},
		{"path change", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: self, ObjectNew: moved}, q)
		}, "one-x,one-y,two-x"},
		{"status only", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: self, ObjectNew: statusOnly}, q)
		}, ""},
		{"path unset", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Update(ctx, event.UpdateEvent{ObjectOld: self, ObjectNew: unset}, q)
		}, "one-x,one-y"},
		{"generic", func(q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			h.Generic(ctx, event.GenericEvent{Object: self}, q)
		}, ""},
	}
	for _, tc := range cases {
		q := newQueue()
		tc.fire(q)
		if got := drain(q); got != tc.want {
			t.Errorf("%s: enqueued %q, want %q", tc.name, got, tc.want)
		}
		q.ShutDown()
	}
}

// TestChannelsForAgent: an Agent change enqueues the channels that bind that
// Agent in its namespace, and no others, through the spec.agentRef index.
func TestChannelsForAgent(t *testing.T) {
	bound := func(ns, name, agent string) *kaalmv1beta1.AgentChannel {
		return &kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       kaalmv1beta1.AgentChannelSpec{AgentRef: kaalmv1beta1.LocalObjectReference{Name: agent}},
		}
	}
	objs := []client.Object{
		bound("team-a", "a-1", "alpha"), bound("team-a", "a-2", "alpha"),
		bound("team-a", "b-1", "beta"), bound("team-a", "g-1", "gamma"),
		bound("team-b", "other-ns", "alpha"),
	}
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "alpha"}}
	ctx := context.Background()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelAgentRef, channelAgentRefIndex).
		WithObjects(objs...).Build()
	var names []string
	for _, req := range (&AgentChannelReconciler{Client: c}).channelsForAgent(ctx, agent) {
		if req.Namespace != "team-a" {
			t.Errorf("enqueued %s outside the Agent's namespace", req)
		}
		names = append(names, req.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "a-1,a-2" {
		t.Errorf("channelsForAgent enqueued %q, want %q", got, "a-1,a-2")
	}

	// The lookup goes through the index: a cache without it cannot answer.
	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	if reqs := (&AgentChannelReconciler{Client: unindexed}).channelsForAgent(ctx, agent); reqs != nil {
		t.Errorf("channelsForAgent without the %s index answered %v; want the indexed lookup", IndexChannelAgentRef, reqs)
	}
}

// TestChannelsForSecret: a Secret change enqueues the channels in its
// namespace whose credentials reference it, through the referenced-Secret
// index.
func TestChannelsForSecret(t *testing.T) {
	const ref = "shared"
	cbURL := "https://example.com/hook"
	webhook := func(ns, name string, auth kaalmv1beta1.ChannelAuth, mutate func(*kaalmv1beta1.AgentChannelWebhook)) *kaalmv1beta1.AgentChannel {
		ch := &kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: kaalmv1beta1.AgentChannelSpec{
				Webhook: &kaalmv1beta1.AgentChannelWebhook{Path: "/channels/" + ns + "/" + name, Auth: auth},
			},
		}
		if mutate != nil {
			mutate(ch.Spec.Webhook)
		}
		return ch
	}
	bearer := func(name string) kaalmv1beta1.ChannelAuth {
		return kaalmv1beta1.ChannelAuth{Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: name, Key: "token"}}
	}
	callback := func(withURL bool) func(*kaalmv1beta1.AgentChannelWebhook) {
		return func(w *kaalmv1beta1.AgentChannelWebhook) {
			if withURL {
				w.CallbackURL = &cbURL
			}
			auth := bearer(ref)
			w.CallbackAuth = &auth
		}
	}
	objs := []client.Object{
		webhook("team-a", "bearer", bearer(ref), nil),
		webhook("team-a", "hmac", kaalmv1beta1.ChannelAuth{Type: "hmac", HMAC: &kaalmv1beta1.ChannelHMAC{
			Header: "X-Sig", SecretRef: kaalmv1beta1.SecretKeyReference{Name: ref, Key: "key"}}}, nil),
		webhook("team-a", "callback", bearer("inbound"), callback(true)),
		&kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "discord"},
			Spec: kaalmv1beta1.AgentChannelSpec{
				Type: kaalmv1beta1.ChannelTypeDiscord,
				Discord: &kaalmv1beta1.AgentChannelDiscord{
					Path: "/channels/team-a/discord", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: ref}},
			},
		},
		webhook("team-a", "callback-no-url", bearer("inbound"), callback(false)),
		webhook("team-a", "other", bearer("other"), nil),
		webhook("team-b", "other-ns", bearer(ref), nil),
	}
	key := types.NamespacedName{Namespace: "team-a", Name: ref}
	ctx := context.Background()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelSecretRef, channelSecretRefIndex).
		WithObjects(objs...).Build()
	var names []string
	for _, req := range (&AgentChannelReconciler{Client: c}).channelsForSecret(ctx, key) {
		if req.Namespace != "team-a" {
			t.Errorf("enqueued %s outside the Secret's namespace", req)
		}
		names = append(names, req.Name)
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "bearer,callback,discord,hmac"; got != want {
		t.Errorf("channelsForSecret enqueued %q, want %q", got, want)
	}

	// The lookup goes through the index: a cache without it cannot answer.
	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	if reqs := (&AgentChannelReconciler{Client: unindexed}).channelsForSecret(ctx, key); reqs != nil {
		t.Errorf("channelsForSecret without the %s index answered %v; want the indexed lookup", IndexChannelSecretRef, reqs)
	}
}

func TestPathConflictLoser(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	cases := []struct {
		name               string
		aName              string
		aCreated           time.Time
		bName              string
		bCreated           time.Time
		wantLoser, wantWin string
	}{
		{"b newer", "ch-a", t0, "ch-b", t1, "ch-b", "ch-a"},
		{"b older (clock went back)", "ch-a", t1, "ch-b", t0, "ch-a", "ch-b"},
		{"tie, b later by name", "ch-a", t0, "ch-b", t0, "ch-b", "ch-a"},
		{"tie, a later by name", "ch-z", t0, "ch-b", t0, "ch-z", "ch-b"},
	}
	for _, c := range cases {
		loser, winner := pathConflictLoser(c.aName, c.aCreated, c.bName, c.bCreated)
		if loser != c.wantLoser || winner != c.wantWin {
			t.Errorf("%s: loser=%s winner=%s, want loser=%s winner=%s", c.name, loser, winner, c.wantLoser, c.wantWin)
		}
	}
}

func TestChannel_CredentialsMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-cred", nil)
	mkWorkloadAgent(t, "ch-agent-cred", "chc-cred", nil)
	mkChannel(t, "ch-cred", "ch-agent-cred", "/channels/default/ch-cred", nil) // secret never created
	expectChannelReady(t, "ch-cred", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

func TestChannel_InvalidCallbackURL(t *testing.T) {
	mkWorkloadClass(t, "chc-cb", nil)
	mkWorkloadAgent(t, "ch-agent-cb", "chc-cb", nil)
	mkChannelSecret(t, "ch-cb-secret")
	badURL := "http://example.com/hook" // not https
	mkChannel(t, "ch-cb", "ch-agent-cb", "/channels/default/ch-cb", func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.CallbackURL = &badURL
		ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
			Type:      "bearer",
			SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "ch-cb-secret", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-cb", metav1.ConditionFalse, kaalmv1beta1.ReasonInvalidCallbackURL)
}

// Rule 22: a callback host that does not resolve at reconcile time leaves
// the channel Ready (the gateway re-checks before every dial) and raises one
// CallbackHostUnresolved Warning naming the host, not one per pass.
func TestChannel_UnresolvableCallbackHostWarns(t *testing.T) {
	mkWorkloadClass(t, "chc-nxcb", nil)
	mkWorkloadAgent(t, "ch-agent-nxcb", "chc-nxcb", nil)
	mkCallbackSecret(t, "ch-nxcb-secret", "kaalm-callback-typo.invalid",
		map[string][]byte{"token": []byte("hook-token")})
	cbURL := "https://kaalm-callback-typo.invalid/hook" // .invalid never resolves (RFC 6761)
	mkChannel(t, "ch-nxcb", "ch-agent-nxcb", "/channels/default/ch-nxcb", func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.CallbackURL = &cbURL
		ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
			Type:      "bearer",
			SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "ch-nxcb-secret", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-nxcb", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	unresolvedEvents := func() ([]corev1.Event, error) {
		var events corev1.EventList
		if err := testAPIReader.List(ctxT(), &events, client.InNamespace("default")); err != nil {
			return nil, err
		}
		var out []corev1.Event
		for _, ev := range events.Items {
			if ev.InvolvedObject.Name == "ch-nxcb" && ev.Reason == kaalmv1beta1.ReasonCallbackHostUnresolved {
				out = append(out, ev)
			}
		}
		return out, nil
	}
	eventually(t, func() error {
		evs, err := unresolvedEvents()
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return errString("no CallbackHostUnresolved event yet")
		}
		if evs[0].Type != corev1.EventTypeWarning || !strings.Contains(evs[0].Message, "kaalm-callback-typo.invalid") {
			return errString(fmt.Sprintf("event %s %q, want a Warning naming the host", evs[0].Type, evs[0].Message))
		}
		return nil
	})

	// More passes through the Agent watch must not repeat the Warning.
	var agent kaalmv1beta1.Agent
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-agent-nxcb"}, &agent); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if agent.Labels == nil {
			agent.Labels = map[string]string{}
		}
		agent.Labels["touch"] = fmt.Sprint(i)
		if err := testClient.Update(ctxT(), &agent); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-agent-nxcb"}, &agent); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Second)
	evs, err := unresolvedEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Count > 1 {
		count := int32(0)
		if len(evs) > 0 {
			count = evs[0].Count
		}
		t.Errorf("repeat passes re-emitted the Warning: %d events, count %d", len(evs), count)
	}
	expectChannelReady(t, "ch-nxcb", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
}

func TestChannel_DegradedWhenAgentDegraded(t *testing.T) {
	mkWorkloadClass(t, "chc-deg", nil)
	mkWorkloadAgent(t, "ch-agent-deg", "chc-deg", func(ag *kaalmv1beta1.Agent) {
		// Image outside the allowlist degrades the agent.
		ag.Spec.Image = "evil.example/x:v1"
	})
	expectAgentPhase(t, "ch-agent-deg", kaalmv1beta1.AgentDegraded)
	mkChannelSecret(t, "ch-deg-secret")
	mkChannel(t, "ch-deg", "ch-agent-deg", "/channels/default/ch-deg", nil)
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-deg"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != kaalmv1beta1.ChannelDegraded {
			return errString("phase=" + string(ch.Status.Phase) + " want Degraded")
		}
		return nil
	})
}

// mkAsyncRecord creates an async response record for the default-namespace
// channel, expired an hour ago or expiring in an hour.
func mkAsyncRecord(t *testing.T, channel, name string, expired bool) {
	t.Helper()
	expiry := time.Now().Add(time.Hour)
	if expired {
		expiry = time.Now().Add(-time.Hour)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testSystemNamespace,
			Labels: map[string]string{
				kaalmv1beta1.LabelChannelNamespace: "default",
				kaalmv1beta1.LabelChannelName:      channel,
			},
			Annotations: map[string]string{
				kaalmv1beta1.AnnotationExpiresAt: expiry.UTC().Format(time.RFC3339),
			},
		},
		Data: map[string]string{},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create async cm: %v", err)
	}
}

// expectAsyncPruned waits for the expired record to go and checks the live
// one stays.
func expectAsyncPruned(t *testing.T, expired, live string) {
	t.Helper()
	eventually(t, func() error {
		var cm corev1.ConfigMap
		err := testAPIReader.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: expired}, &cm)
		if !apierrors.IsNotFound(err) {
			return errString("expired record not pruned")
		}
		return nil
	})
	var cm corev1.ConfigMap
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: live}, &cm); err != nil {
		t.Errorf("live record must survive the prune: %v", err)
	}
}

// A channel that fails validation still prunes its expired records: it can
// hold records written while it was Ready.
func TestChannel_PruneExpiredAsyncConfigMapsWhileInvalid(t *testing.T) {
	mkWorkloadClass(t, "chc-prune-inv", nil)
	mkWorkloadAgent(t, "ch-agent-prune-inv", "chc-prune-inv", nil)
	// No channel Secret, so validation fails with CredentialsMissing.
	mkAsyncRecord(t, "ch-prune-inv", "kaalm-async-inv-expired", true)
	mkAsyncRecord(t, "ch-prune-inv", "kaalm-async-inv-live", false)

	mkChannel(t, "ch-prune-inv", "ch-agent-prune-inv", "/channels/default/ch-prune-inv", nil)
	expectChannelReady(t, "ch-prune-inv", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
	expectAsyncPruned(t, "kaalm-async-inv-expired", "kaalm-async-inv-live")
}

// A channel whose Agent is gone still prunes its expired records.
func TestChannel_PruneExpiredAsyncConfigMapsAgentNotFound(t *testing.T) {
	mkChannelSecret(t, "ch-prune-noagent-secret")
	mkAsyncRecord(t, "ch-prune-noagent", "kaalm-async-noagent-expired", true)
	mkAsyncRecord(t, "ch-prune-noagent", "kaalm-async-noagent-live", false)

	mkChannel(t, "ch-prune-noagent", "no-such-agent", "/channels/default/ch-prune-noagent", nil)
	expectChannelReady(t, "ch-prune-noagent", metav1.ConditionFalse, kaalmv1beta1.ReasonAgentNotFound)
	expectAsyncPruned(t, "kaalm-async-noagent-expired", "kaalm-async-noagent-live")
}

// agentNotFoundFakeChannel is a channel with its finalizer set whose Agent
// does not exist, for driving the AgentNotFound exit on a fake client.
func agentNotFoundFakeChannel() *kaalmv1beta1.AgentChannel {
	return &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ch-agent-missing", Namespace: "default",
			Finalizers: []string{kaalmv1beta1.ChannelFinalizer},
		},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "missing"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/default/ch-agent-missing",
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "ch-agent-missing-secret", Key: "token"},
				},
			},
		},
	}
}

// No watch event re-runs a channel after its Agent is deleted, so the
// AgentNotFound exit re-checks every minute to prune records that expire
// later.
func TestChannel_AgentNotFoundRequeuesEveryMinute(t *testing.T) {
	ch := agentNotFoundFakeChannel()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex).
		WithObjects(ch).WithStatusSubresource(ch).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	res, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ch-agent-missing"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, time.Minute)
	}
}

// A prune error on a failing pass is returned, but the Ready=False status
// write has already happened.
func TestChannel_NotReadyPruneErrorKeepsStatus(t *testing.T) {
	ch := agentNotFoundFakeChannel()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex).
		WithObjects(ch).WithStatusSubresource(ch).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					return errString("list failed")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	key := types.NamespacedName{Namespace: "default", Name: "ch-agent-missing"}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err == nil {
		t.Fatal("want the prune error, got nil")
	}
	if res != (reconcile.Result{}) {
		t.Errorf("result = %+v, want zero alongside an error", res)
	}
	var got kaalmv1beta1.AgentChannel
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != kaalmv1beta1.ChannelFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != kaalmv1beta1.ReasonAgentNotFound {
		t.Errorf("Ready = %+v, want False/AgentNotFound", cond)
	}
}

// A prune error on a valid pass is returned, but the pass's status write
// (Ready=True, phase) has already happened, as on a failing pass.
func TestChannel_ValidPruneErrorKeepsStatus(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ch-valid", Namespace: "default"}}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-valid-secret", Namespace: "default", Labels: channelCredentialLabels()},
		Data:       map[string][]byte{"token": []byte("hook-token")},
	}
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ch-valid", Namespace: "default",
			Finalizers: []string{kaalmv1beta1.ChannelFinalizer},
		},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-valid"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/default/ch-valid",
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "ch-valid-secret", Key: "token"},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex).
		WithIndex(&kaalmv1beta1.AgentChannel{}, IndexChannelPath, channelPathIndex).
		WithObjects(agent, sec, ch).WithStatusSubresource(ch).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					return errString("list failed")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: testSystemNamespace}
	key := types.NamespacedName{Namespace: "default", Name: "ch-valid"}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err == nil {
		t.Fatal("want the prune error, got nil")
	}
	if res != (reconcile.Result{}) {
		t.Errorf("result = %+v, want zero alongside an error", res)
	}
	var got kaalmv1beta1.AgentChannel
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != kaalmv1beta1.ReasonAgentReachable {
		t.Errorf("Ready = %+v, want True/AgentReachable", cond)
	}
	if got.Status.Phase != kaalmv1beta1.ChannelActive {
		t.Errorf("phase = %q, want Active", got.Status.Phase)
	}
}

// A normal pass prunes a record with no parseable expiry once its
// creationTimestamp is past twice the TTL, the rule the orphan pruner uses.
func TestChannel_PruneFallsBackToCreationTime(t *testing.T) {
	now := time.Now()
	objs := []client.Object{
		asyncRecord("kaalm-async-fb-bad-old", "default", "ch-fb", "not-a-time", now.Add(-3*time.Hour)),
		asyncRecord("kaalm-async-fb-missing-old", "default", "ch-fb", "", now.Add(-3*time.Hour)),
		asyncRecord("kaalm-async-fb-bad-young", "default", "ch-fb", "not-a-time", now.Add(-90*time.Minute)),
		asyncRecord("kaalm-async-fb-expired", "default", "ch-fb", rfc(now.Add(-time.Minute)), now.Add(-61*time.Minute)),
		asyncRecord("kaalm-async-fb-live", "default", "ch-fb", rfc(now.Add(30*time.Minute)), now.Add(-30*time.Minute)),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex).WithObjects(objs...).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: orphanTestNS}
	ch := &kaalmv1beta1.AgentChannel{ObjectMeta: metav1.ObjectMeta{Name: "ch-fb", Namespace: "default"}}
	if err := r.pruneAsyncConfigMaps(context.Background(), ch, false); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for name, want := range map[string]bool{
		"kaalm-async-fb-bad-old":     false,
		"kaalm-async-fb-missing-old": false,
		"kaalm-async-fb-bad-young":   true,
		"kaalm-async-fb-expired":     false,
		"kaalm-async-fb-live":        true,
	} {
		if got := recordExists(t, c, name); got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}

func TestChannel_PruneExpiredAsyncConfigMaps(t *testing.T) {
	mkWorkloadClass(t, "chc-prune", nil)
	mkWorkloadAgent(t, "ch-agent-prune", "chc-prune", nil)
	mkChannelSecret(t, "ch-prune-secret")

	mkAsyncRecord(t, "ch-prune", "kaalm-async-expired-1", true)
	mkAsyncRecord(t, "ch-prune", "kaalm-async-live-1", false)

	mkChannel(t, "ch-prune", "ch-agent-prune", "/channels/default/ch-prune", nil)
	expectChannelReady(t, "ch-prune", metav1.ConditionTrue, "")
	expectAsyncPruned(t, "kaalm-async-expired-1", "kaalm-async-live-1")
}

func TestChannel_DeleteHandshake(t *testing.T) {
	mkWorkloadClass(t, "chc-del", nil)
	mkWorkloadAgent(t, "ch-agent-del", "chc-del", nil)
	mkChannelSecret(t, "ch-del-secret")
	mkChannel(t, "ch-del", "ch-agent-del", "/channels/default/ch-del", nil)
	expectChannelReady(t, "ch-del", metav1.ConditionTrue, "")

	// A live async record that only the finalizer sweep may remove.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kaalm-async-del-1", Namespace: testSystemNamespace,
			Labels: map[string]string{
				kaalmv1beta1.LabelChannelNamespace: "default",
				kaalmv1beta1.LabelChannelName:      "ch-del",
			},
			Annotations: map[string]string{
				kaalmv1beta1.AnnotationExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create async cm: %v", err)
	}

	var ch kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &ch); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &ch); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	// Step 1: the reconciler announces Terminating and holds.
	eventually(t, func() error {
		var got kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got); err != nil {
			return err
		}
		if got.Status.Phase != kaalmv1beta1.ChannelTerminating {
			return errString("phase=" + string(got.Status.Phase) + " want Terminating")
		}
		return nil
	})

	// Steps 2-3: play the gateway and confirm disconnection.
	eventually(t, func() error {
		var got kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[kaalmv1beta1.AnnotationChannelDisconnected] = kaalmv1beta1.AnnotationTrue
		return testClient.Update(ctxT(), &got)
	})

	// Steps 5-6: sweep and release.
	eventually(t, func() error {
		var got kaalmv1beta1.AgentChannel
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got)
		if !apierrors.IsNotFound(err) {
			return errString("channel not yet finalized")
		}
		return nil
	})
	var sweptCM corev1.ConfigMap
	err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: "kaalm-async-del-1"}, &sweptCM)
	if !apierrors.IsNotFound(err) {
		t.Error("finalizer sweep must remove the channel's async records")
	}
}

// ---- AgentChannel ensureCredentialRole: secret refs change -> Role updated ----

func TestChannel_CredentialRoleGrowsWithSecretRefs(t *testing.T) {
	mkWorkloadClass(t, "chc-role", nil)
	mkWorkloadAgent(t, "ch-agent-role", "chc-role", nil)
	mkChannelSecret(t, "ch-role-secret")
	mkChannel(t, "ch-role", "ch-agent-role", "/channels/default/ch-role", nil)
	expectChannelReady(t, "ch-role", metav1.ConditionTrue, "")

	roleName := "kaalm-channel-ch-role-creds"
	eventually(t, func() error {
		var role rbacv1.Role
		if err := testAPIReader.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: roleName}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 || len(role.Rules[0].ResourceNames) != 1 {
			return errString("initial role not scoped to one secret")
		}
		return nil
	})

	// Add an HMAC secret ref: the Role's resourceNames must grow to include it.
	mkChannelSecret(t, "ch-role-hmac")
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-role"}, &ch); err != nil {
			return err
		}
		ch.Spec.Webhook.Auth.HMAC = &kaalmv1beta1.ChannelHMAC{
			Header:    "X-Sig",
			SecretRef: kaalmv1beta1.SecretKeyReference{Name: "ch-role-hmac", Key: "token"},
		}
		return testClient.Update(ctxT(), &ch)
	})
	eventually(t, func() error {
		var role rbacv1.Role
		if err := testAPIReader.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: roleName}, &role); err != nil {
			return err
		}
		names := append([]string(nil), role.Rules[0].ResourceNames...)
		sort.Strings(names)
		if len(names) != 2 || names[0] != "ch-role-hmac" || names[1] != "ch-role-secret" {
			return errString("role resourceNames did not grow to both secrets")
		}
		return nil
	})
}

// ---- AgentChannel reconcileDelete: no finalizer is a no-op ----

func TestChannel_DeleteBeforeFinalizerIsNoop(t *testing.T) {
	// A channel with the deletion timestamp already set but no finalizer must
	// short-circuit reconcileDelete without touching status.
	r := &AgentChannelReconciler{Client: testClient, OperatorNamespace: testSystemNamespace}
	now := metav1.Now()
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ephemeral", Namespace: "default", DeletionTimestamp: &now},
	}
	res, err := r.reconcileDelete(context.Background(), ch)
	if err != nil || res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("no-finalizer delete should be a clean no-op: res=%+v err=%v", res, err)
	}
}

// ---- AgentChannel: system-namespace guard ----

func TestChannel_SystemNamespaceForbidden(t *testing.T) {
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-sys", Namespace: testSystemNamespace},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "whatever"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/" + testSystemNamespace + "/ch-sys",
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "s", Key: "token"},
				},
			},
		},
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "ch-sys"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonSystemNamespaceForbidden {
			return errString("SystemNamespaceForbidden not set")
		}
		if got.Status.Phase != kaalmv1beta1.ChannelFailed {
			return errString("phase=" + string(got.Status.Phase) + ", want Failed (rule 28)")
		}
		return nil
	})
	// No Role is ever written in the operator namespace.
	for _, name := range []string{"kaalm-channel-ch-sys-check", "kaalm-channel-ch-sys-creds"} {
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: testSystemNamespace, Name: name}, &rbacv1.Role{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("Role %s in the operator namespace: err = %v, want NotFound", name, err)
		}
	}
}

// ---- AgentChannel: rule 25 callbackAuth Secret checks ----

func mkCallbackChannel(t *testing.T, name, agentName, cbSecret string) {
	t.Helper()
	cbURL := "https://example.com/hook"
	mkChannel(t, name, agentName, "/channels/default/"+name, func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.CallbackURL = &cbURL
		ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
			Type:      "bearer",
			SecretRef: &kaalmv1beta1.SecretKeyReference{Name: cbSecret, Key: "token"},
		}
	})
}

func TestChannel_CallbackAuthSecretMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-cbmiss", nil)
	mkWorkloadAgent(t, "ch-agent-cbmiss", "chc-cbmiss", nil)
	mkChannelSecret(t, "ch-cbmiss-secret") // inbound Secret only
	mkCallbackChannel(t, "ch-cbmiss", "ch-agent-cbmiss", "ch-cbmiss-callback")
	expectChannelReady(t, "ch-cbmiss", metav1.ConditionFalse, kaalmv1beta1.ReasonCallbackAuthMissing)
}

func TestChannel_CallbackAuthKeyMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-cbkey", nil)
	mkWorkloadAgent(t, "ch-agent-cbkey", "chc-cbkey", nil)
	mkChannelSecret(t, "ch-cbkey-secret")
	mkCallbackSecret(t, "ch-cbkey-callback", "example.com", map[string][]byte{"other": []byte("x")})
	mkCallbackChannel(t, "ch-cbkey", "ch-agent-cbkey", "ch-cbkey-callback")
	expectChannelReady(t, "ch-cbkey", metav1.ConditionFalse, kaalmv1beta1.ReasonCallbackAuthMissing)
}

func TestChannel_CallbackAuthKeyEmpty(t *testing.T) {
	mkWorkloadClass(t, "chc-cbempty", nil)
	mkWorkloadAgent(t, "ch-agent-cbempty", "chc-cbempty", nil)
	mkChannelSecret(t, "ch-cbempty-secret")
	mkCallbackSecret(t, "ch-cbempty-callback", "example.com", map[string][]byte{"token": {}})
	mkCallbackChannel(t, "ch-cbempty", "ch-agent-cbempty", "ch-cbempty-callback")
	expectChannelReady(t, "ch-cbempty", metav1.ConditionFalse, kaalmv1beta1.ReasonCallbackAuthInvalid)
}

// A callbackAuth block that names no Secret for its type is malformed. CRD
// CEL rejects it at apply, so this drives validateSecrets directly.
func TestValidateSecrets_CallbackAuthMalformed(t *testing.T) {
	inbound := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "in", Namespace: "default", Labels: channelCredentialLabels()},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	r := &AgentChannelReconciler{Client: fake.NewClientBuilder().WithObjects(inbound).Build()}
	cbURL := "https://example.com/hook"
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{
			Auth: kaalmv1beta1.ChannelAuth{
				Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "in", Key: "token"},
			},
			CallbackURL:  &cbURL,
			CallbackAuth: &kaalmv1beta1.ChannelAuth{Type: "hmac"},
		}},
	}
	if reason, _, _ := r.validateSecrets(ctxT(), ch); reason != kaalmv1beta1.ReasonCallbackAuthInvalid {
		t.Errorf("reason = %q, want %q", reason, kaalmv1beta1.ReasonCallbackAuthInvalid)
	}
}

// ---- AgentChannel: HMAC secret missing fails validation ----

func TestChannel_HMACSecretMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-hmac", nil)
	mkWorkloadAgent(t, "ch-agent-hmac", "chc-hmac", nil)
	mkChannelSecret(t, "ch-hmac-inbound")
	mkChannel(t, "ch-hmac", "ch-agent-hmac", "/channels/default/ch-hmac", func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.Auth.SecretRef = &kaalmv1beta1.SecretKeyReference{Name: "ch-hmac-inbound", Key: "token"}
		ch.Spec.Webhook.Auth.HMAC = &kaalmv1beta1.ChannelHMAC{
			Header:    "X-Sig",
			SecretRef: kaalmv1beta1.SecretKeyReference{Name: "ch-hmac-missing", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-hmac", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

// TestChannel_PruneSkipsNonAsyncConfigMap covers the prune loop's skip of a
// labeled ConfigMap that is not an async-response record.
func TestChannel_PruneSkipsNonAsyncConfigMap(t *testing.T) {
	mkWorkloadClass(t, "chc-skip", nil)
	mkWorkloadAgent(t, "ch-agent-skip", "chc-skip", nil)
	mkChannelSecret(t, "ch-skip-secret")

	// A ConfigMap carrying this channel's labels but NOT the kaalm-async- name
	// prefix: the prune must leave it untouched.
	other := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unrelated-config", Namespace: testSystemNamespace,
			Labels: map[string]string{
				kaalmv1beta1.LabelChannelNamespace: "default",
				kaalmv1beta1.LabelChannelName:      "ch-skip",
			},
			Annotations: map[string]string{
				kaalmv1beta1.AnnotationExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := testClient.Create(ctxT(), other); err != nil {
		t.Fatalf("create configmap: %v", err)
	}

	mkChannel(t, "ch-skip", "ch-agent-skip", "/channels/default/ch-skip", nil)
	expectChannelReady(t, "ch-skip", metav1.ConditionTrue, "")

	// Give the reconciler time to run a prune pass, then confirm survival.
	time.Sleep(500 * time.Millisecond)
	var got corev1.ConfigMap
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: "unrelated-config"}, &got); err != nil {
		t.Errorf("a non-async ConfigMap must not be pruned: %v", err)
	}
}

// ---- validateCallbackURL (rule 22, reconcile-time half) ----

func TestValidateCallbackURL(t *testing.T) {
	allowPrivate := callbackpolicy.New([]string{"10.0.0.0/8", "127.0.0.0/8", "169.254.169.254/32"})
	cases := []struct {
		name      string
		url       string
		allowlist callbackpolicy.Policy
		wantBad   bool
	}{
		{"not https", "http://example.com/hook", callbackpolicy.Policy{}, true},
		{"parse error", "://no-scheme", callbackpolicy.Policy{}, true},
		{"empty host", "https://", callbackpolicy.Policy{}, true},
		{"loopback literal", "https://127.0.0.1/hook", callbackpolicy.Policy{}, true},
		{"public literal ok", "https://8.8.8.8/hook", callbackpolicy.Policy{}, false},
		{"unresolvable passes", "https://nonexistent.invalid/hook", callbackpolicy.Policy{}, false},
		{"resolved name blocked", "https://internal.example/hook", callbackpolicy.Policy{}, true},
		{"private blocked by default", "https://10.1.2.3/hook", callbackpolicy.Policy{}, true},
		{"private allowed when allowlisted", "https://10.1.2.3/hook", allowPrivate, false},
		{"loopback blocked even when allowlisted", "https://127.0.0.1/hook", allowPrivate, true},
		{"metadata blocked even when allowlisted", "https://169.254.169.254/hook", allowPrivate, true},
	}
	for _, c := range cases {
		reason, _, _ := validateCallbackURL(context.Background(), c.url, c.allowlist, fakeLookupIP)
		bad := reason != ""
		if bad != c.wantBad {
			t.Errorf("%s: validateCallbackURL(%q) bad=%v, want %v (reason=%q)", c.name, c.url, bad, c.wantBad, reason)
		}
		if bad && reason != kaalmv1beta1.ReasonInvalidCallbackURL {
			t.Errorf("%s: reason=%q, want InvalidCallbackURL", c.name, reason)
		}
	}
}

// fakeLookupIP answers IP literals as themselves, maps internal.example to a
// private address, and fails every other name, so rule 22 tests never touch
// real DNS.
func fakeLookupIP(_ context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if host == "internal.example" {
		return []net.IP{net.ParseIP("10.0.0.7")}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// Rule 22 reports the host it could not resolve, so the reconciler can warn
// about it without failing the channel.
func TestValidateCallbackURL_ReportsUnresolvedHost(t *testing.T) {
	ctx := context.Background()
	reason, _, unresolved := validateCallbackURL(ctx, "https://nonexistent.invalid/hook", callbackpolicy.Policy{}, fakeLookupIP)
	if reason != "" || unresolved != "nonexistent.invalid" {
		t.Errorf("unresolvable host: reason=%q unresolved=%q, want clean with the host", reason, unresolved)
	}
	reason, _, unresolved = validateCallbackURL(ctx, "https://8.8.8.8/hook", callbackpolicy.Policy{}, fakeLookupIP)
	if reason != "" || unresolved != "" {
		t.Errorf("resolvable host: reason=%q unresolved=%q, want both empty", reason, unresolved)
	}
}

// The CallbackHostUnresolved Warning fires when a channel's callback host
// first fails to resolve, not on every one-minute pass. It fires again after
// the host resolves and fails again, or when callbackUrl names a new host.
func TestChannel_CallbackHostUnresolvedWarnsOnChange(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := &AgentChannelReconciler{Recorder: rec}
	ch := &kaalmv1beta1.AgentChannel{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default", UID: "uid-1"}}

	drain := func() []string {
		var got []string
		for {
			select {
			case e := <-rec.Events:
				got = append(got, e)
			default:
				return got
			}
		}
	}

	r.noteCallbackResolution(ch, "typo.invalid")
	r.noteCallbackResolution(ch, "typo.invalid")
	r.noteCallbackResolution(ch, "typo.invalid")
	got := drain()
	if len(got) != 1 {
		t.Fatalf("three passes on the same unresolved host emitted %d events, want 1: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "Warning "+kaalmv1beta1.ReasonCallbackHostUnresolved) ||
		!strings.Contains(got[0], "typo.invalid") {
		t.Errorf("event = %q, want a CallbackHostUnresolved Warning naming the host", got[0])
	}

	r.noteCallbackResolution(ch, "other.invalid")
	if got := drain(); len(got) != 1 || !strings.Contains(got[0], "other.invalid") {
		t.Errorf("a new unresolved host must warn again: %v", got)
	}

	r.noteCallbackResolution(ch, "") // resolves now
	if got := drain(); len(got) != 0 {
		t.Errorf("a resolving host emits nothing: %v", got)
	}
	r.noteCallbackResolution(ch, "other.invalid")
	if got := drain(); len(got) != 1 {
		t.Errorf("failing again after resolving must warn again: %v", got)
	}

	r.forgetCallbackResolution(ch)
	r.noteCallbackResolution(ch, "other.invalid")
	if got := drain(); len(got) != 1 {
		t.Errorf("a forgotten channel warns afresh: %v", got)
	}
}

// ---- channel health reduction ----

// fakeChannelHealth serves canned per-replica channel health.
type fakeChannelHealth struct {
	reachable []ReplicaChannelHealth
	total     int
	err       error
}

func (f fakeChannelHealth) NamespaceChannelHealth(
	context.Context, string,
) ([]ReplicaChannelHealth, int, error) {
	return f.reachable, f.total, f.err
}

// chTestPath is the webhook path shared across the channel-health unit tests.
const chTestPath = "/channels/default/x"

func newChannelAt() *kaalmv1beta1.AgentChannel {
	return &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			Webhook: &kaalmv1beta1.AgentChannelWebhook{Path: chTestPath},
		},
	}
}

func platformCond(ch *kaalmv1beta1.AgentChannel) *metav1.Condition {
	return condition(ch.Status.Conditions, kaalmv1beta1.ConditionPlatformConnected)
}

func TestReduceChannelHealth_NoDataPreservesCondition(t *testing.T) {
	// total==0 is rule 4: no condition written.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{total: 0}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatalf("rule 4: no PlatformConnected condition expected, got %+v", platformCond(ch))
	}

	// err also preserves.
	r = &AgentChannelReconciler{Health: fakeChannelHealth{
		reachable: []ReplicaChannelHealth{{}}, total: 1, err: errString("boom"),
	}}
	ch = newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("error result must preserve the existing condition")
	}
}

func TestReduceChannelHealth_Success(t *testing.T) {
	path := chTestPath
	// Two success replicas with different timestamps: the newer wins (newerHealth).
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 2,
		reachable: []ReplicaChannelHealth{
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-01-01T00:00:00Z")}}},
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-02-01T00:00:00Z")}}},
		},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != kaalmv1beta1.ReasonWebhookReady {
		t.Fatalf("rule 1 success expected True/WebhookReady, got %+v", c)
	}
}

func TestReduceChannelHealth_Failure(t *testing.T) {
	path := chTestPath
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			Channels: map[string]ChannelHealthState{path: {
				State:     healthStateFailure,
				Reason:    strptr("Timeout"),
				LastError: strptr("dial tcp: i/o timeout"),
				Timestamp: strptr("2026-01-01T00:00:00Z"),
			}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Timeout" ||
		c.Message != "dial tcp: i/o timeout" {
		t.Fatalf("rule 2 failure expected False/Timeout, got %+v", c)
	}
}

func TestReduceChannelHealth_NoRecentTraffic(t *testing.T) {
	path := chTestPath
	// A replica up longer than its window, with only an empty state: rule 3.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now().Add(-time.Hour),
			WindowSeconds: 60,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != kaalmv1beta1.ReasonNoRecentTraffic {
		t.Fatalf("rule 3 expected Unknown/NoRecentTraffic, got %+v", c)
	}
}

func TestReduceChannelHealth_DefaultReturnsNoCondition(t *testing.T) {
	path := chTestPath
	// Window not yet full and all empty: rule 4 default -> no condition.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now(),
			WindowSeconds: 3600,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("rule 4 default must leave the condition unset")
	}
}

func TestNewerHealth(t *testing.T) {
	cases := []struct {
		name string
		a, b ChannelHealthState
		want bool
	}{
		{"both nil", ChannelHealthState{}, ChannelHealthState{}, true},
		{"a nil b set", ChannelHealthState{}, ChannelHealthState{Timestamp: strptr("x")}, false},
		{"a set b nil", ChannelHealthState{Timestamp: strptr("x")}, ChannelHealthState{}, true},
		{"a newer", ChannelHealthState{Timestamp: strptr("2026-02")}, ChannelHealthState{Timestamp: strptr("2026-01")}, true},
		{"a older", ChannelHealthState{Timestamp: strptr("2026-01")}, ChannelHealthState{Timestamp: strptr("2026-02")}, false},
	}
	for _, c := range cases {
		if got := newerHealth(c.a, c.b); got != c.want {
			t.Errorf("%s: newerHealth = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAuthSecretNames_DedupAndHMAC(t *testing.T) {
	cb := "https://example.com/hook"
	ch := &kaalmv1beta1.AgentChannel{
		Spec: kaalmv1beta1.AgentChannelSpec{
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Auth: kaalmv1beta1.ChannelAuth{
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "inbound", Key: "t"},
					HMAC:      &kaalmv1beta1.ChannelHMAC{SecretRef: kaalmv1beta1.SecretKeyReference{Name: "hmac-sec", Key: "s"}},
				},
				CallbackURL: &cb,
				CallbackAuth: &kaalmv1beta1.ChannelAuth{
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "inbound", Key: "t"}, // duplicate name
				},
			},
		},
	}
	names := authSecretNames(ch)
	// Sorted, deduped: hmac-sec, inbound.
	if len(names) != 2 || names[0] != "hmac-sec" || names[1] != "inbound" {
		t.Errorf("authSecretNames = %v, want [hmac-sec inbound]", names)
	}
}

// ---- Platform channels (since v0.7.0): rules 39 and 40 ----

// A valid Ed25519 public key in hex: 32 bytes.
const testDiscordPublicKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// mkPlatformSecret creates an opted-in Secret (rule 45) with data.
func mkPlatformSecret(t *testing.T, name string, data map[string][]byte) {
	t.Helper()
	mkDefaultSecret(t, name, channelCredentialLabels(), nil, data)
}

func mkDiscordChannel(t *testing.T, name, agentName, path, secretName string) {
	t.Helper()
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: agentName},
			Type:     kaalmv1beta1.ChannelTypeDiscord,
			Discord: &kaalmv1beta1.AgentChannelDiscord{
				Path:           path,
				CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: secretName},
			},
		},
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create discord channel %s: %v", name, err)
	}
}

func TestChannel_DiscordValidBecomesReady(t *testing.T) {
	mkWorkloadClass(t, "chc-dc", nil)
	mkWorkloadAgent(t, "ch-agent-dc", "chc-dc", nil)
	mkPlatformSecret(t, "ch-dc-creds", map[string][]byte{"publicKey": []byte(testDiscordPublicKey)})
	mkDiscordChannel(t, "ch-dc", "ch-agent-dc", "/channels/default/ch-dc", "ch-dc-creds")

	expectChannelReady(t, "ch-dc", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	// The credential Role is scoped to the one credentialsRef Secret.
	var role rbacv1.Role
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-channel-ch-dc-creds"}, &role); err != nil {
		t.Fatalf("credential Role missing: %v", err)
	}
	if names := role.Rules[0].ResourceNames; len(names) != 1 || names[0] != "ch-dc-creds" {
		t.Errorf("Role not scoped to the credentials Secret: %v", names)
	}
	// The default contentOption lands from the CRD.
	var ch kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-dc"}, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Spec.Discord.ContentOption != "message" {
		t.Errorf("contentOption default = %q, want message", ch.Spec.Discord.ContentOption)
	}
}

func TestChannel_DiscordPublicKeyMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-dcm", nil)
	mkWorkloadAgent(t, "ch-agent-dcm", "chc-dcm", nil)
	mkPlatformSecret(t, "ch-dcm-creds", map[string][]byte{"botToken": []byte("x")})
	mkDiscordChannel(t, "ch-dcm", "ch-agent-dcm", "/channels/default/ch-dcm", "ch-dcm-creds")
	expectChannelReady(t, "ch-dcm", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

func TestChannel_DiscordPublicKeyMalformed(t *testing.T) {
	mkWorkloadClass(t, "chc-dci", nil)
	mkWorkloadAgent(t, "ch-agent-dci", "chc-dci", nil)
	mkPlatformSecret(t, "ch-dci-creds", map[string][]byte{"publicKey": []byte("not-hex")})
	mkDiscordChannel(t, "ch-dci", "ch-agent-dci", "/channels/default/ch-dci", "ch-dci-creds")
	expectChannelReady(t, "ch-dci", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsInvalid)

	// Right length of hex but the wrong size is still invalid.
	mkPlatformSecret(t, "ch-dci-short", map[string][]byte{"publicKey": []byte("abcd")})
	mkDiscordChannel(t, "ch-dci2", "ch-agent-dci", "/channels/default/ch-dci2", "ch-dci-short")
	expectChannelReady(t, "ch-dci2", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsInvalid)
}

func TestChannel_WhatsAppRequiresEveryKey(t *testing.T) {
	mkWorkloadClass(t, "chc-wa", nil)
	mkWorkloadAgent(t, "ch-agent-wa", "chc-wa", nil)
	mkPlatformSecret(t, "ch-wa-creds", map[string][]byte{
		"verifyToken": []byte("v"), "appSecret": []byte("s"),
	})
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-wa", Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-wa"},
			Type:     kaalmv1beta1.ChannelTypeWhatsApp,
			WhatsApp: &kaalmv1beta1.AgentChannelWhatsApp{
				Path:           "/channels/default/ch-wa",
				CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "ch-wa-creds"},
				PhoneNumberID:  "106540352242922",
			},
		},
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create whatsapp channel: %v", err)
	}
	expectChannelReady(t, "ch-wa", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)

	// Adding the missing key makes it Ready at once: the Secret's change
	// re-runs the channel.
	editSecret(t, "ch-wa-creds", func(s *corev1.Secret) { s.Data["accessToken"] = []byte("t") })
	expectChannelReady(t, "ch-wa", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
}

func TestChannel_TypeBlockMismatchRejectedByCEL(t *testing.T) {
	mkWorkloadClass(t, "chc-cel", nil)
	mkWorkloadAgent(t, "ch-agent-cel", "chc-cel", nil)
	cases := map[string]kaalmv1beta1.AgentChannelSpec{
		"discord type with a webhook block": {
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-cel"},
			Type:     kaalmv1beta1.ChannelTypeDiscord,
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/default/cel-a",
				Auth: kaalmv1beta1.ChannelAuth{Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "s", Key: "k"}},
			},
		},
		"default type with a discord block": {
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-cel"},
			Discord: &kaalmv1beta1.AgentChannelDiscord{
				Path: "/channels/default/cel-b", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "s"},
			},
		},
		"two blocks": {
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-cel"},
			Type:     kaalmv1beta1.ChannelTypeWhatsApp,
			WhatsApp: &kaalmv1beta1.AgentChannelWhatsApp{
				Path: "/channels/default/cel-c", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "s"}, PhoneNumberID: "1",
			},
			Discord: &kaalmv1beta1.AgentChannelDiscord{
				Path: "/channels/default/cel-c", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "s"},
			},
		},
		"discord path under /v1/": {
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-cel"},
			Type:     kaalmv1beta1.ChannelTypeDiscord,
			Discord: &kaalmv1beta1.AgentChannelDiscord{
				Path: "/v1/cel-d", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "s"},
			},
		},
		"guild id is not a snowflake": {
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-cel"},
			Type:     kaalmv1beta1.ChannelTypeDiscord,
			Discord: &kaalmv1beta1.AgentChannelDiscord{
				Path: "/channels/default/cel-e", CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "s"},
				GuildID: func() *string { s := "guild"; return &s }(),
			},
		},
	}
	i := 0
	for name, spec := range cases {
		i++
		ch := &kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ch-cel-%d", i), Namespace: "default"},
			Spec:       spec,
		}
		err := testClient.Create(ctxT(), ch)
		if err == nil || !apierrors.IsInvalid(err) {
			t.Errorf("%s: want an Invalid rejection from the apiserver, got %v", name, err)
		}
	}
}

func TestChannel_PathConflictAcrossTypes(t *testing.T) {
	mkWorkloadClass(t, "chc-xt", nil)
	mkWorkloadAgent(t, "ch-agent-xt", "chc-xt", nil)
	mkChannelSecret(t, "ch-xt-a-secret")
	mkChannel(t, "ch-xt-a", "ch-agent-xt", "/channels/default/ch-xt", nil)
	expectChannelReady(t, "ch-xt-a", metav1.ConditionTrue, "")

	mkPlatformSecret(t, "ch-xt-creds", map[string][]byte{"publicKey": []byte(testDiscordPublicKey)})
	mkDiscordChannel(t, "ch-xt-b", "ch-agent-xt", "/channels/default/ch-xt", "ch-xt-creds")
	// The two channels usually share a creationTimestamp second, so the tie
	// goes to the lower name; the stored timestamps decide, not creation order.
	expectPathConflict(t, "ch-xt-a", "ch-xt-b")
}

// TestChannel_UnchangedPassWritesNoStatus: a pass that changes nothing in the
// status writes nothing. The reconciler runs every channel every minute and
// on every Agent change, so an unconditional status write was the largest
// single write the controller made under load.
func TestChannel_UnchangedPassWritesNoStatus(t *testing.T) {
	mkWorkloadClass(t, "chc-quiet", nil)
	mkWorkloadAgent(t, "ch-agent-quiet", "chc-quiet", nil)
	mkChannelSecret(t, "ch-quiet-secret")
	mkChannel(t, "ch-quiet", "ch-agent-quiet", "/channels/default/ch-quiet", nil)
	expectChannelReady(t, "ch-quiet", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
	key := types.NamespacedName{Namespace: "default", Name: "ch-quiet"}
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), key, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != kaalmv1beta1.ChannelActive {
			return errString("phase=" + string(ch.Status.Phase))
		}
		return nil
	})
	var settled kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), key, &settled); err != nil {
		t.Fatal(err)
	}

	// Touching the Agent re-enqueues the channel through the Agent watch;
	// the pass finds nothing to change.
	var agent kaalmv1beta1.Agent
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-agent-quiet"}, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Labels == nil {
		agent.Labels = map[string]string{}
	}
	agent.Labels["touch"] = "1"
	if err := testClient.Update(ctxT(), &agent); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	var after kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), key, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != settled.ResourceVersion {
		t.Errorf("an unchanged pass rewrote the channel: resourceVersion %s -> %s", settled.ResourceVersion, after.ResourceVersion)
	}
}

// A Role with the channel's credential Role name that the channel does not
// control is neither rewritten nor adopted: the channel reports
// ChildConflict, and deleting the Role lets it recover.
func TestChannel_UnownedCredentialRoleIsChildConflict(t *testing.T) {
	mkWorkloadClass(t, "chc-own-role", nil)
	mkWorkloadAgent(t, "ch-agent-own-role", "chc-own-role", nil)
	mkChannelSecret(t, "ch-own-role-secret")
	roleName := "kaalm-channel-ch-own-role-creds"
	foreign := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: "default"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"},
		}},
	}
	if err := testClient.Create(ctxT(), foreign); err != nil {
		t.Fatal(err)
	}
	mkChannel(t, "ch-own-role", "ch-agent-own-role", "/channels/default/ch-own-role", func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.Auth.SecretRef = &kaalmv1beta1.SecretKeyReference{Name: "ch-own-role-secret", Key: "token"}
	})
	expectChannelReady(t, "ch-own-role", metav1.ConditionFalse, kaalmv1beta1.ReasonChildConflict)

	var role rbacv1.Role
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: roleName}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || role.Rules[0].Resources[0] != "configmaps" || metav1.GetControllerOf(&role) != nil {
		t.Fatalf("the foreign Role was rewritten or adopted: %+v", role)
	}

	if err := testClient.Delete(ctxT(), &role); err != nil {
		t.Fatal(err)
	}
	expectChannelReady(t, "ch-own-role", metav1.ConditionTrue, "")
}

// A controller binding to the credential Role that the channel controls,
// as an older release created it, is deleted; the gateway binding stays.
func TestChannel_ControllerCredsBindingRemoved(t *testing.T) {
	mkWorkloadClass(t, "chc-ccb-legacy", nil)
	mkWorkloadAgent(t, "ch-agent-ccb-legacy", "chc-ccb-legacy", nil)
	mkChannelSecret(t, "ch-ccb-legacy-secret")
	mkChannel(t, "ch-ccb-legacy", "ch-agent-ccb-legacy", "/channels/default/ch-ccb-legacy", nil)
	expectChannelReady(t, "ch-ccb-legacy", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	var ch kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-ccb-legacy"}, &ch); err != nil {
		t.Fatal(err)
	}
	name := channelControllerCredsBindingName("ch-ccb-legacy")
	legacy := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: "kaalm-channel-ch-ccb-legacy-creds"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: controllerServiceAccount, Namespace: testSystemNamespace,
		}},
	}
	if err := controllerutil.SetControllerReference(&ch, legacy, testClient.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Create(ctxT(), legacy); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}

	eventually(t, func() error {
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &rbacv1.RoleBinding{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return errString("the controller RoleBinding to the credential Role still exists")
	})
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{
		Namespace: "default", Name: "kaalm-channel-ch-ccb-legacy-creds-gateway",
	}, &rbacv1.RoleBinding{}); err != nil {
		t.Errorf("gateway RoleBinding missing: %v", err)
	}
}

// A RoleBinding with the old controller binding's name that the channel does
// not control is not the channel's child: it is left alone and does not
// block the channel.
func TestChannel_UnownedCredsControllerBindingLeftAlone(t *testing.T) {
	name := channelControllerCredsBindingName("ch-ccb-foreign")
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: "someone-elses-role"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: "someone", Namespace: "default",
		}},
	}
	if err := testClient.Create(ctxT(), foreign); err != nil {
		t.Fatal(err)
	}
	mkWorkloadClass(t, "chc-ccb-foreign", nil)
	mkWorkloadAgent(t, "ch-agent-ccb-foreign", "chc-ccb-foreign", nil)
	mkChannelSecret(t, "ch-ccb-foreign-secret")
	mkChannel(t, "ch-ccb-foreign", "ch-agent-ccb-foreign", "/channels/default/ch-ccb-foreign", nil)
	expectChannelReady(t, "ch-ccb-foreign", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	var rb rbacv1.RoleBinding
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &rb); err != nil {
		t.Fatalf("the foreign RoleBinding was removed: %v", err)
	}
	if rb.RoleRef != foreign.RoleRef || len(rb.Subjects) != 1 || rb.Subjects[0] != foreign.Subjects[0] ||
		metav1.GetControllerOf(&rb) != nil {
		t.Fatalf("the foreign RoleBinding was rewritten or adopted: %+v", rb)
	}
}

// ---- Rules 45 and 46: channel Secrets opt in, bearer callbacks bind hosts ----

// assertCheckRole waits for the controller-only check Role of a channel to
// list exactly names, bound to the controller's ServiceAccount alone.
func assertCheckRole(t *testing.T, channel string, names []string) {
	t.Helper()
	name := "kaalm-channel-" + channel + "-check"
	eventually(t, func() error {
		role, err := getRole(name)
		if err != nil {
			return err
		}
		if len(role.Rules) != 1 {
			return errString(fmt.Sprintf("check Role has %d rules, want 1", len(role.Rules)))
		}
		got := append([]string(nil), role.Rules[0].ResourceNames...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(names, ",") {
			return errString(fmt.Sprintf("check Role lists %v, want %v", got, names))
		}
		var rb rbacv1.RoleBinding
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &rb); err != nil {
			return err
		}
		if len(rb.Subjects) != 1 || rb.Subjects[0].Name != controllerServiceAccount {
			return errString(fmt.Sprintf("check RoleBinding subjects %+v, want the controller only", rb.Subjects))
		}
		return nil
	})
}

// expectCredsRole waits for the -creds Role to grant exactly names (none
// means a Role with no rules).
func expectCredsRole(t *testing.T, channel string, names []string) {
	t.Helper()
	eventually(t, func() error {
		got, err := credsRoleNames(channel)
		if err != nil {
			return err
		}
		if strings.Join(got, ",") != strings.Join(names, ",") {
			return errString(fmt.Sprintf("-creds Role grants %v, want %v", got, names))
		}
		return nil
	})
}

// readyMessage returns the channel's Ready condition message.
func readyMessage(t *testing.T, name string) string {
	t.Helper()
	var ch kaalmv1beta1.AgentChannel
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
		t.Fatal(err)
	}
	if c := condition(ch.Status.Conditions, kaalmv1beta1.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}

// Rule 45: a Secret without the opt-in label leaves the channel not Ready
// and out of the gateway's Role; labeling it brings the channel up.
func TestChannel_UnlabeledSecretNotOptedIn(t *testing.T) {
	mkWorkloadClass(t, "chc-nolabel", nil)
	mkWorkloadAgent(t, "ch-agent-nolabel", "chc-nolabel", nil)
	mkUnlabeledChannelSecret(t, "ch-nolabel-secret")
	mkChannel(t, "ch-nolabel", "ch-agent-nolabel", "/channels/default/ch-nolabel", nil)

	expectChannelReady(t, "ch-nolabel", metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	if msg := readyMessage(t, "ch-nolabel"); !strings.Contains(msg, kaalmv1beta1.LabelChannelCredential) {
		t.Errorf("message %q does not name the label", msg)
	}
	expectCredsRole(t, "ch-nolabel", nil)
	assertCheckRole(t, "ch-nolabel", []string{"ch-nolabel-secret"})

	editSecret(t, "ch-nolabel-secret", func(s *corev1.Secret) { s.Labels = channelCredentialLabels() })
	expectChannelReady(t, "ch-nolabel", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
	expectCredsRole(t, "ch-nolabel", []string{"ch-nolabel-secret"})
}

// Rule 45 covers the callbackAuth Secret: the inbound Secret is granted, the
// unlabeled callback Secret is not, and the message says which reference.
func TestChannel_CallbackAuthSecretNotOptedIn(t *testing.T) {
	mkWorkloadClass(t, "chc-cbnolabel", nil)
	mkWorkloadAgent(t, "ch-agent-cbnolabel", "chc-cbnolabel", nil)
	mkChannelSecret(t, "ch-cbnolabel-secret")
	mkUnlabeledChannelSecret(t, "ch-cbnolabel-callback")
	mkCallbackChannel(t, "ch-cbnolabel", "ch-agent-cbnolabel", "ch-cbnolabel-callback")

	expectChannelReady(t, "ch-cbnolabel", metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	if msg := readyMessage(t, "ch-cbnolabel"); !strings.HasPrefix(msg, "callbackAuth: ") {
		t.Errorf("message %q, want the callbackAuth: prefix", msg)
	}
	expectCredsRole(t, "ch-cbnolabel", []string{"ch-cbnolabel-secret"})
	assertCheckRole(t, "ch-cbnolabel", []string{"ch-cbnolabel-callback", "ch-cbnolabel-secret"})
}

// Rule 45 runs before the key checks of rule 40: an unlabeled platform Secret
// with a missing or malformed key reports SecretNotOptedIn and names no key.
func TestChannel_DiscordSecretNotOptedIn(t *testing.T) {
	mkWorkloadClass(t, "chc-dcnl", nil)
	mkWorkloadAgent(t, "ch-agent-dcnl", "chc-dcnl", nil)
	mkDefaultSecret(t, "ch-dcnl-creds", nil, nil, map[string][]byte{"publicKey": []byte("not-hex")})
	mkDiscordChannel(t, "ch-dcnl", "ch-agent-dcnl", "/channels/default/ch-dcnl", "ch-dcnl-creds")

	expectChannelReady(t, "ch-dcnl", metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	if msg := readyMessage(t, "ch-dcnl"); strings.Contains(msg, "publicKey") {
		t.Errorf("message %q names a key of an unlabeled Secret", msg)
	}
	expectCredsRole(t, "ch-dcnl", nil)
}

func TestChannel_WhatsAppSecretNotOptedIn(t *testing.T) {
	mkWorkloadClass(t, "chc-wanl", nil)
	mkWorkloadAgent(t, "ch-agent-wanl", "chc-wanl", nil)
	mkDefaultSecret(t, "ch-wanl-creds", nil, nil, map[string][]byte{"verifyToken": []byte("v")})
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-wanl", Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "ch-agent-wanl"},
			Type:     kaalmv1beta1.ChannelTypeWhatsApp,
			WhatsApp: &kaalmv1beta1.AgentChannelWhatsApp{
				Path:           "/channels/default/ch-wanl",
				CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "ch-wanl-creds"},
				PhoneNumberID:  "106540352242922",
			},
		},
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create whatsapp channel: %v", err)
	}
	expectChannelReady(t, "ch-wanl", metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	msg := readyMessage(t, "ch-wanl")
	for _, key := range []string{"verifyToken", "appSecret", "accessToken"} {
		if strings.Contains(msg, key) {
			t.Errorf("message %q names the key %s of an unlabeled Secret", msg, key)
		}
	}
	expectCredsRole(t, "ch-wanl", nil)
}

// Removing the label from a Secret a Ready channel uses shrinks the gateway's
// Role to nothing and reports SecretNotOptedIn. This is also the upgrade
// case: a -creds Role written by the previous release starts populated.
func TestChannel_LabelRemovedShrinksGatewayRole(t *testing.T) {
	mkWorkloadClass(t, "chc-unlabel", nil)
	mkWorkloadAgent(t, "ch-agent-unlabel", "chc-unlabel", nil)
	mkChannelSecret(t, "ch-unlabel-secret")
	mkChannel(t, "ch-unlabel", "ch-agent-unlabel", "/channels/default/ch-unlabel", nil)
	expectChannelReady(t, "ch-unlabel", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
	expectCredsRole(t, "ch-unlabel", []string{"ch-unlabel-secret"})

	editSecret(t, "ch-unlabel-secret", func(s *corev1.Secret) {
		delete(s.Labels, kaalmv1beta1.LabelChannelCredential)
	})
	expectChannelReady(t, "ch-unlabel", metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	expectCredsRole(t, "ch-unlabel", nil)
	// The bindings stay; they grant nothing while the Role has no rules.
	var rb rbacv1.RoleBinding
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-channel-ch-unlabel-creds-gateway"}, &rb); err != nil {
		t.Errorf("gateway RoleBinding removed: %v", err)
	}
}

// A channel whose Agent does not exist still keeps its credential Role to
// the labeled Secrets, and the Role follows a label change.
func TestChannel_AgentNotFoundCredentialRoleFollowsLabel(t *testing.T) {
	mkChannelSecret(t, "ch-notready-label-secret")
	mkChannel(t, "ch-notready-label", "no-such-agent", "/channels/default/ch-notready-label", nil)
	expectChannelReady(t, "ch-notready-label", metav1.ConditionFalse, kaalmv1beta1.ReasonAgentNotFound)
	expectCredsRole(t, "ch-notready-label", []string{"ch-notready-label-secret"})
	assertCheckRole(t, "ch-notready-label", []string{"ch-notready-label-secret"})

	editSecret(t, "ch-notready-label-secret", func(s *corev1.Secret) {
		delete(s.Labels, kaalmv1beta1.LabelChannelCredential)
	})
	expectCredsRole(t, "ch-notready-label", nil)
	expectChannelReady(t, "ch-notready-label", metav1.ConditionFalse, kaalmv1beta1.ReasonAgentNotFound)
}

// A channel whose Secret does not exist yet turns Ready once the Secret is
// created, with no edit to the channel.
func TestChannel_SecretCreatedAfterChannelBecomesReady(t *testing.T) {
	mkWorkloadClass(t, "chc-late", nil)
	mkWorkloadAgent(t, "ch-agent-late", "chc-late", nil)
	mkChannel(t, "ch-late", "ch-agent-late", "/channels/default/ch-late", nil)
	expectChannelReady(t, "ch-late", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)

	mkChannelSecret(t, "ch-late-secret")
	expectChannelReady(t, "ch-late", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)
}

// Deleting the Secret a Ready channel uses takes the channel down at once.
func TestChannel_SecretDeletedTakesChannelDown(t *testing.T) {
	mkWorkloadClass(t, "chc-secdel", nil)
	mkWorkloadAgent(t, "ch-agent-secdel", "chc-secdel", nil)
	mkChannelSecret(t, "ch-secdel-secret")
	mkChannel(t, "ch-secdel", "ch-agent-secdel", "/channels/default/ch-secdel", nil)
	expectChannelReady(t, "ch-secdel", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ch-secdel-secret"}}
	if err := testClient.Delete(ctxT(), sec); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	expectChannelReady(t, "ch-secdel", metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

// Rule 46: a bearer callbackAuth Secret must list the callbackUrl host in
// kaalm.io/callback-hosts. Listing it brings the channel up; moving the
// callbackUrl to another host takes it down again.
func TestChannel_CallbackHostNotApproved(t *testing.T) {
	mkWorkloadClass(t, "chc-cbhost", nil)
	mkWorkloadAgent(t, "ch-agent-cbhost", "chc-cbhost", nil)
	mkChannelSecret(t, "ch-cbhost-secret")
	mkCallbackSecret(t, "ch-cbhost-callback", "other.invalid", map[string][]byte{"token": []byte("t")})
	// .invalid never resolves (RFC 6761): a channel that passes rule 46 stays
	// Ready with only the CallbackHostUnresolved Warning.
	cbURL := "https://kaalm-cbhost.invalid:8443/hook"
	mkChannel(t, "ch-cbhost", "ch-agent-cbhost", "/channels/default/ch-cbhost", func(ch *kaalmv1beta1.AgentChannel) {
		ch.Spec.Webhook.CallbackURL = &cbURL
		ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
			Type:      "bearer",
			SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "ch-cbhost-callback", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-cbhost", metav1.ConditionFalse, kaalmv1beta1.ReasonCallbackHostNotApproved)
	if msg := readyMessage(t, "ch-cbhost"); !strings.Contains(msg, "kaalm-cbhost.invalid") ||
		!strings.HasPrefix(msg, "callbackAuth: ") {
		t.Errorf("message %q, want the callbackAuth: prefix and the host", msg)
	}

	// Listed in another case, port ignored.
	editSecret(t, "ch-cbhost-callback", func(s *corev1.Secret) {
		s.Annotations[kaalmv1beta1.AnnotationCallbackHosts] = "other.invalid, KAALM-CBHOST.invalid"
	})
	expectChannelReady(t, "ch-cbhost", metav1.ConditionTrue, kaalmv1beta1.ReasonAgentReachable)

	// Editing the callbackUrl to an unlisted host is caught too.
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-cbhost"}, &ch); err != nil {
			return err
		}
		moved := "https://kaalm-elsewhere.invalid/hook"
		ch.Spec.Webhook.CallbackURL = &moved
		return testClient.Update(ctxT(), &ch)
	})
	expectChannelReady(t, "ch-cbhost", metav1.ConditionFalse, kaalmv1beta1.ReasonCallbackHostNotApproved)
}

// validateSecrets in isolation: the order of rules 45 and 46 against the key
// checks, the HMAC exemption from rule 46, and the opted-in set it returns.
func TestValidateSecrets_OptInAndCallbackHosts(t *testing.T) {
	labeled := channelCredentialLabels()
	sec := func(name string, labels, annotations map[string]string, data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, Annotations: annotations},
			Data:       data,
		}
	}
	token := map[string][]byte{"token": []byte("t")}
	hosts := func(h string) map[string]string { return map[string]string{kaalmv1beta1.AnnotationCallbackHosts: h} }
	objs := []client.Object{
		sec("in", labeled, nil, token),
		sec("in-bad-label", map[string]string{kaalmv1beta1.LabelChannelCredential: "True"}, nil, token),
		sec("cb-ok", labeled, hosts("hooks.example.com"), token),
		sec("cb-nohosts", labeled, nil, token),
		sec("cb-unlabeled-nokey", nil, hosts("hooks.example.com"), map[string][]byte{"other": []byte("x")}),
		sec("cb-nokey", labeled, hosts("other.example.com"), map[string][]byte{"other": []byte("x")}),
	}
	r := &AgentChannelReconciler{Client: fake.NewClientBuilder().WithObjects(objs...).Build()}
	channel := func(inbound, cbType, cbSecret, cbURL string) *kaalmv1beta1.AgentChannel {
		ch := &kaalmv1beta1.AgentChannel{
			ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
			Spec: kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Auth: kaalmv1beta1.ChannelAuth{
					Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: inbound, Key: "token"},
				},
			}},
		}
		if cbSecret == "" {
			return ch
		}
		ch.Spec.Webhook.CallbackURL = &cbURL
		ref := kaalmv1beta1.SecretKeyReference{Name: cbSecret, Key: "token"}
		if cbType == "hmac" {
			ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
				Type: "hmac", HMAC: &kaalmv1beta1.ChannelHMAC{Header: "X-Sig", SecretRef: ref},
			}
		} else {
			ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{Type: "bearer", SecretRef: &ref}
		}
		return ch
	}
	cases := []struct {
		name        string
		ch          *kaalmv1beta1.AgentChannel
		wantReason  string
		wantOptedIn []string
		notInMsg    string
	}{
		{"labeled inbound", channel("in", "", "", ""), "", []string{"in"}, ""},
		{"label value not exactly true", channel("in-bad-label", "", "", ""),
			kaalmv1beta1.ReasonSecretNotOptedIn, nil, ""},
		{"bearer callback, host listed", channel("in", "bearer", "cb-ok", "https://HOOKS.example.com:8443/cb"),
			"", []string{"cb-ok", "in"}, ""},
		{"bearer callback, no hosts annotation", channel("in", "bearer", "cb-nohosts", "https://hooks.example.com/cb"),
			kaalmv1beta1.ReasonCallbackHostNotApproved, []string{"cb-nohosts", "in"}, ""},
		{"bearer callback, host not listed", channel("in", "bearer", "cb-ok", "https://evil.example.com/cb"),
			kaalmv1beta1.ReasonCallbackHostNotApproved, []string{"cb-ok", "in"}, ""},
		{"hmac callback needs no hosts", channel("in", "hmac", "cb-nohosts", "https://hooks.example.com/cb"),
			"", []string{"cb-nohosts", "in"}, ""},
		{"non-https callbackUrl is left to rule 22", channel("in", "bearer", "cb-nohosts", "http://hooks.example.com/cb"),
			"", []string{"cb-nohosts", "in"}, ""},
		{"unlabeled callback reports no keys", channel("in", "bearer", "cb-unlabeled-nokey", "https://hooks.example.com/cb"),
			kaalmv1beta1.ReasonSecretNotOptedIn, []string{"in"}, `"token"`},
		{"host check precedes key checks", channel("in", "bearer", "cb-nokey", "https://hooks.example.com/cb"),
			kaalmv1beta1.ReasonCallbackHostNotApproved, []string{"cb-nokey", "in"}, `"token"`},
		{"missing callback Secret is not opted in", channel("in", "bearer", "cb-absent", "https://hooks.example.com/cb"),
			kaalmv1beta1.ReasonCallbackAuthMissing, []string{"in"}, ""},
	}
	for _, c := range cases {
		reason, msg, optedIn := r.validateSecrets(ctxT(), c.ch)
		if reason != c.wantReason {
			t.Errorf("%s: reason %q (%s), want %q", c.name, reason, msg, c.wantReason)
		}
		if strings.Join(optedIn, ",") != strings.Join(c.wantOptedIn, ",") {
			t.Errorf("%s: opted in %v, want %v", c.name, optedIn, c.wantOptedIn)
		}
		if c.notInMsg != "" && strings.Contains(msg, c.notInMsg) {
			t.Errorf("%s: message %q names %s", c.name, msg, c.notInMsg)
		}
	}
}

// The async-record index keys a kaalm-async- ConfigMap by the channel its
// labels name; any other ConfigMap is not indexed.
func TestAsyncChannelIndex(t *testing.T) {
	now := time.Now()
	labelled := asyncRecord("kaalm-async-idx", "team-a", "ch", "", now)
	if got := asyncChannelIndex(labelled); len(got) != 1 || got[0] != "team-a/ch" {
		t.Errorf("index = %q, want [team-a/ch]", got)
	}
	unlabelled := asyncRecord("kaalm-async-idx", "", "", "", now)
	if got := asyncChannelIndex(unlabelled); got != nil {
		t.Errorf("a record without channel labels is indexed as %q", got)
	}
	halfLabelled := asyncRecord("kaalm-async-idx", "team-a", "ch", "", now)
	delete(halfLabelled.Labels, kaalmv1beta1.LabelChannelNamespace)
	if got := asyncChannelIndex(halfLabelled); got != nil {
		t.Errorf("a record with one channel label is indexed as %q", got)
	}
	other := asyncRecord("kaalm-budget-x", "team-a", "ch", "", now)
	if got := asyncChannelIndex(other); got != nil {
		t.Errorf("a ConfigMap without the kaalm-async- prefix is indexed as %q", got)
	}
}

// A channel pass reads only its own channel's records, through the index,
// instead of every ConfigMap in the operator namespace.
func TestPruneAsyncConfigMaps_ListsThroughChannelIndex(t *testing.T) {
	now := time.Now()
	objs := []client.Object{
		asyncRecord("kaalm-async-ix-expired", "default", "ch-ix", rfc(now.Add(-time.Minute)), now.Add(-time.Hour)),
		asyncRecord("kaalm-async-ix-live", "default", "ch-ix", rfc(now.Add(time.Hour)), now),
		asyncRecord("kaalm-async-ix-other", "default", "ch-other", rfc(now.Add(-time.Minute)), now.Add(-time.Hour)),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithIndex(&corev1.ConfigMap{}, IndexAsyncChannel, asyncChannelIndex).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					lo := (&client.ListOptions{}).ApplyOptions(opts)
					if lo.FieldSelector == nil {
						return fmt.Errorf("a ConfigMap list without the channel index scans the whole namespace")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := &AgentChannelReconciler{Client: c, OperatorNamespace: orphanTestNS}
	ch := &kaalmv1beta1.AgentChannel{ObjectMeta: metav1.ObjectMeta{Name: "ch-ix", Namespace: "default"}}
	if err := r.pruneAsyncConfigMaps(context.Background(), ch, false); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for name, want := range map[string]bool{
		"kaalm-async-ix-expired": false,
		"kaalm-async-ix-live":    true,
		"kaalm-async-ix-other":   true,
	} {
		if got := recordExists(t, c, name); got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}
