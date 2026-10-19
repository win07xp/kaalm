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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/eventrec"
)

// expectAgentReadyMessage waits until the Agent's Ready condition has reason
// and a message that holds want and passes check.
func expectAgentReadyMessage(t *testing.T, name, reason, want string, check func(msg string) error) {
	t.Helper()
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag); err != nil {
			return err
		}
		c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != reason {
			return fmt.Errorf("Ready = %+v, want reason %s", c, reason)
		}
		if !strings.Contains(c.Message, want) {
			return fmt.Errorf("Ready message (%d bytes) does not hold %q", len(c.Message), want)
		}
		if check != nil {
			return check(c.Message)
		}
		return nil
	})
}

// spec.agentClassRef.name has no length limit in the schema. The message
// quotes it bounded.
func TestAgent_LongClassRefQuotedInPart(t *testing.T) {
	mkWorkloadAgent(t, "long-classref-agent", strings.Repeat("c", 40000), nil)
	expectAgentReadyMessage(t, "long-classref-agent", kaalmv1beta1.ReasonInvalidReference,
		"AgentClass \"", nil)
	expectAgentReadyMessage(t, "long-classref-agent", kaalmv1beta1.ReasonInvalidReference,
		"(first 256 of 40000 bytes) does not exist", nil)
}

func TestAgent_LongExistingClaimQuotedInPart(t *testing.T) {
	mkWorkloadClass(t, "wc-long-claim", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
	})
	claim := strings.Repeat("p", 40000)
	mkWorkloadAgent(t, "long-claim-agent", "wc-long-claim", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Persistence.ExistingClaim = &claim
	})
	expectAgentReadyMessage(t, "long-claim-agent", kaalmv1beta1.ReasonExistingClaimNotFound,
		"(first 256 of 40000 bytes) not found in namespace", nil)
}

func TestAgent_LongImagePullSecretQuotedInPart(t *testing.T) {
	mkWorkloadClass(t, "wc-long-pull", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: strings.Repeat("s", 40000)}}
	})
	mkWorkloadAgent(t, "long-pull-agent", "wc-long-pull", nil)
	expectAgentReadyMessage(t, "long-pull-agent", kaalmv1beta1.ReasonImagePullSecretMissing,
		"(first 256 of 40000 bytes) missing", nil)
}

func TestAgent_LongHandlerConfigMapQuotedInPart(t *testing.T) {
	mkWorkloadClass(t, "wc-long-hcm", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.AllowHandlerMounts = true
	})
	mkWorkloadAgent(t, "long-hcm-agent", "wc-long-hcm", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Handler = &kaalmv1beta1.AgentHandler{
			ConfigMapRef: kaalmv1beta1.LocalObjectReference{Name: strings.Repeat("h", 40000)},
		}
	})
	expectAgentReadyMessage(t, "long-hcm-agent", kaalmv1beta1.ReasonHandlerConfigMapNotFound,
		"(first 256 of 40000 bytes) not found", nil)
}

// The kubelet's waiting message has no length limit. Ready copies it cut at
// the Event note limit.
func TestAgent_LongCrashLoopMessageCut(t *testing.T) {
	mkWorkloadClass(t, "wc-long-crash", nil)
	pod := provisionRunningAgent(t, "long-crash-agent", "wc-long-crash")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "agent",
		RestartCount: crashLoopThreshold,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: strings.Repeat("m", 40000),
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	limit := len("container agent: ") + eventrec.MaxNoteBytes
	expectAgentReadyMessage(t, "long-crash-agent", "CrashLoopBackOff", "container agent: ", func(msg string) error {
		if len(msg) > limit || !strings.HasSuffix(msg, "... (truncated)") {
			return fmt.Errorf("Ready message is %d bytes, want at most %d ending with the truncation marker",
				len(msg), limit)
		}
		return nil
	})
}
