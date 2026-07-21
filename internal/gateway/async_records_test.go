/*
Copyright 2026.

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

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// nsTeamA is a sample tenant namespace reused across these records tests.
const nsTeamA = "team-a"

func TestAsyncCMName(t *testing.T) {
	if got := asyncCMName("abc-123"); got != "agentry-async-abc-123" {
		t.Errorf("asyncCMName = %q", got)
	}
}

func TestKubeAsyncRecords_Lifecycle(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	recs := &KubeAsyncRecords{Client: client, OperatorNamespace: "agentry-system"}
	ctx := context.Background()

	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: nsTeamA},
		Spec: agentryv1alpha1.AgentChannelSpec{
			Webhook: agentryv1alpha1.AgentChannelWebhook{Path: "/channels/team-a/hook"},
		},
	}

	// Get before Create: not found, no error.
	if rec, found, err := recs.Get(ctx, "req-1"); err != nil || found || rec != nil {
		t.Fatalf("Get before create: rec=%v found=%v err=%v", rec, found, err)
	}

	expires := time.Now().Add(asyncTTL)
	if err := recs.Create(ctx, "req-1", ch, expires); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify labels and expiry annotation landed.
	cm, err := client.CoreV1().ConfigMaps("agentry-system").Get(ctx, asyncCMName("req-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cm.Labels[agentryv1alpha1.LabelChannelNamespace] != nsTeamA ||
		cm.Labels[agentryv1alpha1.LabelChannelName] != "ch" {
		t.Errorf("labels wrong: %v", cm.Labels)
	}
	if cm.Annotations[agentryv1alpha1.AnnotationExpiresAt] == "" {
		t.Error("expiry annotation missing")
	}

	// Get after Create: found, no payload yet.
	rec, found, err := recs.Get(ctx, "req-1")
	if err != nil || !found {
		t.Fatalf("Get after create: found=%v err=%v", found, err)
	}
	if rec.Payload != nil {
		t.Error("payload must be nil before patch")
	}
	if rec.ChannelNamespace != nsTeamA || rec.ChannelName != "ch" {
		t.Errorf("record channel fields wrong: %+v", rec)
	}

	// Patch adds the payload.
	if err := recs.Patch(ctx, "req-1", []byte(`{"response":"hi"}`)); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	rec, found, err = recs.Get(ctx, "req-1")
	if err != nil || !found || string(rec.Payload) != `{"response":"hi"}` {
		t.Fatalf("payload not read back: %+v found=%v err=%v", rec, found, err)
	}
}

func TestKubeAsyncRecords_CountPending(t *testing.T) {
	ctx := context.Background()
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: nsTeamA},
		Spec:       agentryv1alpha1.AgentChannelSpec{Webhook: agentryv1alpha1.AgentChannelWebhook{Path: "/channels/team-a/hook"}},
	}
	otherCh := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: nsTeamA},
		Spec:       agentryv1alpha1.AgentChannelSpec{Webhook: agentryv1alpha1.AgentChannelWebhook{Path: "/channels/team-a/other"}},
	}

	client := k8sfake.NewSimpleClientset()
	recs := &KubeAsyncRecords{Client: client, OperatorNamespace: "agentry-system"}
	exp := time.Now().Add(asyncTTL)
	if err := recs.Create(ctx, "a1", ch, exp); err != nil {
		t.Fatal(err)
	}
	if err := recs.Create(ctx, "a2", ch, exp); err != nil {
		t.Fatal(err)
	}
	if err := recs.Create(ctx, "b1", otherCh, exp); err != nil {
		t.Fatal(err)
	}

	n, err := recs.CountPending(ctx, nsTeamA, "ch")
	if err != nil || n != 2 {
		t.Errorf("CountPending(ch) = %d err=%v, want 2", n, err)
	}
	n, err = recs.CountPending(ctx, nsTeamA, "other")
	if err != nil || n != 1 {
		t.Errorf("CountPending(other) = %d err=%v, want 1", n, err)
	}
	n, err = recs.CountPending(ctx, nsTeamA, "ghost")
	if err != nil || n != 0 {
		t.Errorf("CountPending(ghost) = %d err=%v, want 0", n, err)
	}
}

func TestKubeCompletionWriter_PatchMailbox(t *testing.T) {
	ctx := context.Background()
	// Seed the mailbox ConfigMap the per-task Role would have created.
	seed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: CompletionMailboxName("fix-42"), Namespace: nsTeamA},
		Data:       map[string]string{},
	}
	client := k8sfake.NewSimpleClientset(seed)
	w := &KubeCompletionWriter{Client: client}

	data := map[string]string{CompletionKeyStatus: CompletionStatusSuccess, "artifact.pr-url": "https://x/1"}
	if err := w.PatchMailbox(ctx, nsTeamA, "fix-42", data); err != nil {
		t.Fatalf("PatchMailbox: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(nsTeamA).Get(ctx, CompletionMailboxName("fix-42"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[CompletionKeyStatus] != CompletionStatusSuccess || cm.Data["artifact.pr-url"] != "https://x/1" {
		t.Errorf("mailbox not patched: %v", cm.Data)
	}
}

// cmErrClientset returns a fake clientset that fails the given verb on
// configmaps, to exercise the error branches.
func cmErrClientset(verb string) *k8sfake.Clientset {
	c := k8sfake.NewSimpleClientset()
	c.PrependReactor(verb, "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected failure")
	})
	return c
}

func TestKubeAsyncRecords_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	// CountPending list error.
	recs := &KubeAsyncRecords{Client: cmErrClientset("list"), OperatorNamespace: "agentry-system"}
	if _, err := recs.CountPending(ctx, "team-a", "ch"); err == nil {
		t.Error("CountPending must surface list errors")
	}

	// Patch error.
	recsP := &KubeAsyncRecords{Client: cmErrClientset("patch"), OperatorNamespace: "agentry-system"}
	if err := recsP.Patch(ctx, "req-1", []byte(`{}`)); err == nil {
		t.Error("Patch must surface errors")
	}

	// Get with a non-NotFound error.
	recsG := &KubeAsyncRecords{Client: cmErrClientset("get"), OperatorNamespace: "agentry-system"}
	if _, _, err := recsG.Get(ctx, "req-1"); err == nil {
		t.Error("Get must surface non-NotFound errors")
	}

	// PatchMailbox error.
	w := &KubeCompletionWriter{Client: cmErrClientset("patch")}
	if err := w.PatchMailbox(ctx, "team-a", "fix", map[string]string{"status": CompletionStatusSuccess}); err == nil {
		t.Error("PatchMailbox must surface errors")
	}
}
