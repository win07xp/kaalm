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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestDeletionBlockedMessage(t *testing.T) {
	if got, want := deletionBlockedMessage([]string{"Agent team-a/support"}),
		"deletion blocked: Agent team-a/support still references it"; got != want {
		t.Errorf("one referrer: %q, want %q", got, want)
	}
	got := deletionBlockedMessage([]string{"Agent team-a/support", "AgentTask team-a/nightly", "AgentClass standard"})
	want := "deletion blocked: 3 objects still reference it, including Agent team-a/support"
	if got != want {
		t.Errorf("three referrers: %q, want %q", got, want)
	}
}

// expectDeletionBlocked waits for Ready=False DeletionBlocked whose message
// names the referrer, then checks the Warning event fired once.
func expectDeletionBlocked(t *testing.T, kind, name string, conds func() []metav1.Condition, referrer string) {
	t.Helper()
	eventually(t, func() error {
		c := condition(conds(), kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonDeletionBlocked {
			return errString("Ready is not False/DeletionBlocked yet")
		}
		if !strings.Contains(c.Message, referrer) {
			return errString("message does not name " + referrer + ": " + c.Message)
		}
		return nil
	})
	expectEvent(t, kind, "", name, kaalmv1beta1.ReasonDeletionBlocked, corev1.EventTypeWarning, referrer)
	time.Sleep(time.Second)
	if n := eventCount(objectEvents(t, kind, "", name, kaalmv1beta1.ReasonDeletionBlocked)); n != 1 {
		t.Errorf("DeletionBlocked emitted %d times, want 1", n)
	}
}

func deleteObject(t *testing.T, obj client.Object) {
	t.Helper()
	if err := testClient.Delete(ctxT(), obj); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s: %v", obj.GetName(), err)
	}
}

func expectGone(t *testing.T, key types.NamespacedName, obj client.Object) {
	t.Helper()
	eventually(t, func() error {
		if err := testClient.Get(ctxT(), key, obj); apierrors.IsNotFound(err) {
			return nil
		}
		return errString(key.Name + " still exists")
	})
}

func TestModelProvider_DeleteBlockedIsVisible(t *testing.T) {
	mkSecret(t, "db-mp-key")
	mkProvider(t, "db-mp", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "db-mp-key", Key: "token"}
	})
	mkClass(t, "db-mp-class", "db-mp")
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "db-mp"}, &mp); err != nil {
			return err
		}
		if len(mp.Finalizers) == 0 {
			return errString("finalizer not added yet")
		}
		return nil
	})

	deleteObject(t, &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-mp"}})
	expectDeletionBlocked(t, "ModelProvider", "db-mp", func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "db-mp"}, &mp)
		return mp.Status.Conditions
	}, "AgentClass db-mp-class")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-mp-class"}})
	expectGone(t, types.NamespacedName{Name: "db-mp"}, &kaalmv1beta1.ModelProvider{})
}

func TestToolProvider_DeleteBlockedIsVisible(t *testing.T) {
	mkToolProvider(t, "db-tp", nil)
	mkWorkloadClass(t, "db-tp-class", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "db-tp"}}
	})
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "db-tp"}, &tp); err != nil {
			return err
		}
		if len(tp.Finalizers) == 0 {
			return errString("finalizer not added yet")
		}
		return nil
	})

	deleteObject(t, &kaalmv1beta1.ToolProvider{ObjectMeta: metav1.ObjectMeta{Name: "db-tp"}})
	expectDeletionBlocked(t, "ToolProvider", "db-tp", toolProviderConditions("db-tp"), "AgentClass db-tp-class")

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-tp-class"}})
	expectGone(t, types.NamespacedName{Name: "db-tp"}, &kaalmv1beta1.ToolProvider{})
}

func TestAgentClass_DeleteBlockedIsVisible(t *testing.T) {
	mkClass(t, "db-ac")
	mkAgent(t, "db-ac-a", "db-ac")
	mkAgent(t, "db-ac-b", "db-ac")
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "db-ac"}, &ac); err != nil {
			return err
		}
		if ac.Status.AgentsInUse != 2 {
			return errString("agentsInUse not 2 yet")
		}
		return nil
	})

	deleteObject(t, &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "db-ac"}})
	classConds := func() []metav1.Condition {
		var ac kaalmv1beta1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "db-ac"}, &ac)
		return ac.Status.Conditions
	}
	expectDeletionBlocked(t, "AgentClass", "db-ac", classConds, "Agent default/db-ac-a")
	if c := condition(classConds(), kaalmv1beta1.ConditionReady); !strings.Contains(c.Message, "2 objects") {
		t.Errorf("message does not carry the referrer count: %q", c.Message)
	}

	for _, n := range []string{"db-ac-a", "db-ac-b"} {
		deleteObject(t, &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"}})
	}
	expectGone(t, types.NamespacedName{Name: "db-ac"}, &kaalmv1beta1.AgentClass{})
}
