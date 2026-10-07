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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestReadyFalseIsNew(t *testing.T) {
	ready := func(status metav1.ConditionStatus, reason string) []metav1.Condition {
		return []metav1.Condition{{Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason}}
	}
	for _, tc := range []struct {
		name  string
		conds []metav1.Condition
		want  bool
	}{
		{"no Ready condition", nil, true},
		{"Ready was True", ready(metav1.ConditionTrue, kaalmv1beta1.ReasonPodRunning), true},
		{"another False reason", ready(metav1.ConditionFalse, "CertificateNotReady"), true},
		{"same False reason", ready(metav1.ConditionFalse, kaalmv1beta1.ReasonInvalidReference), false},
		{"other types only", []metav1.Condition{{Type: kaalmv1beta1.ConditionDegraded,
			Status: metav1.ConditionFalse, Reason: kaalmv1beta1.ReasonInvalidReference}}, true},
	} {
		if got := readyFalseIsNew(tc.conds, kaalmv1beta1.ReasonInvalidReference); got != tc.want {
			t.Errorf("%s: readyFalseIsNew = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A missing AgentClass is reported as a Warning event, not only a condition.
func TestAgent_InvalidReferenceEmitsWarning(t *testing.T) {
	mkWorkloadAgent(t, "ev-noclass-agent", "ev-no-such-class", nil)
	expectEvent(t, "Agent", "default", "ev-noclass-agent", kaalmv1beta1.ReasonInvalidReference,
		corev1.EventTypeWarning, `AgentClass "ev-no-such-class" does not exist`)
}

// A Ready=False check that requeues every notReadyRecheck emits its Warning
// when the reason first appears, not on each requeue.
func TestAgent_NotReadyWarningOncePerReason(t *testing.T) {
	mkWorkloadClass(t, "wc-ev-pull", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "wc-ev-pull-creds"}}
	})
	mkWorkloadAgent(t, "ev-pull-agent", "wc-ev-pull", nil)
	expectEvent(t, "Agent", "default", "ev-pull-agent", kaalmv1beta1.ReasonImagePullSecretMissing,
		corev1.EventTypeWarning, `imagePullSecret "wc-ev-pull-creds" missing`)

	// notReadyRecheck is 500ms in tests: several passes run in this window.
	time.Sleep(2 * time.Second)
	evs := objectEvents(t, "Agent", "default", "ev-pull-agent", kaalmv1beta1.ReasonImagePullSecretMissing)
	if n := eventCount(evs); n != 1 {
		t.Errorf("ImagePullSecretMissing emitted %d times across requeues, want 1", n)
	}
}

func TestAgentTask_InvalidReferenceEmitsWarning(t *testing.T) {
	mkTask(t, "ev-noclass-task", "ev-no-such-task-class", nil)
	expectEvent(t, "AgentTask", "default", "ev-noclass-task", kaalmv1beta1.ReasonInvalidReference,
		corev1.EventTypeWarning, `AgentClass "ev-no-such-task-class" does not exist`)
}

func TestAgentChannel_ValidationFailureEmitsWarning(t *testing.T) {
	mkChannel(t, "ev-orphan-ch", "ev-no-such-agent", "/channels/default/ev-orphan", nil)
	expectEvent(t, "AgentChannel", "default", "ev-orphan-ch", kaalmv1beta1.ReasonAgentNotFound,
		corev1.EventTypeWarning, `Agent "ev-no-such-agent" not found`)
}

func TestAgentClass_InvalidReferenceEmitsWarning(t *testing.T) {
	mkClass(t, "ev-badref-class", "ev-no-such-provider")
	expectEvent(t, "AgentClass", "", "ev-badref-class", kaalmv1beta1.ReasonInvalidReference,
		corev1.EventTypeWarning, `allowedProvider "ev-no-such-provider" does not exist`)
}

// A provider whose credential Secret is missing reports it as a Warning, once.
func TestModelProvider_CredentialsMissingEmitsWarningOnce(t *testing.T) {
	mkProvider(t, "ev-mp-nocred", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "ev-mp-absent", Key: "token"}
	})
	expectEvent(t, "ModelProvider", "", "ev-mp-nocred", kaalmv1beta1.ReasonCredentialsMissing,
		corev1.EventTypeWarning, "ev-mp-absent")
	time.Sleep(time.Second)
	if n := eventCount(objectEvents(t, "ModelProvider", "", "ev-mp-nocred", kaalmv1beta1.ReasonCredentialsMissing)); n != 1 {
		t.Errorf("CredentialsMissing emitted %d times, want 1", n)
	}
}

func TestToolProvider_CredentialsMissingEmitsWarningOnce(t *testing.T) {
	mkToolProvider(t, "ev-tp-nocred", nil)
	expectEvent(t, "ToolProvider", "", "ev-tp-nocred", kaalmv1beta1.ReasonCredentialsMissing,
		corev1.EventTypeWarning, "ev-tp-nocred-key")
	time.Sleep(time.Second)
	if n := eventCount(objectEvents(t, "ToolProvider", "", "ev-tp-nocred", kaalmv1beta1.ReasonCredentialsMissing)); n != 1 {
		t.Errorf("CredentialsMissing emitted %d times, want 1", n)
	}
}
