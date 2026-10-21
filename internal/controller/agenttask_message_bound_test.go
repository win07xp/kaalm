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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/eventrec"
)

// The gateway caps a completion's message plus artifact values at 32 KiB
// (maxArtifactTotalBytes in internal/gateway/completion.go). This test
// writes a longer message straight into the mailbox because, whatever the
// mailbox holds, the task must settle with its stored copies bounded: the
// status copy at maxAgentMessageBytes and each condition message under its
// schema's 32768-character limit.
func TestTask_LongAgentMessageStillSettles(t *testing.T) {
	mkWorkloadClass(t, "tc-long", nil)
	provisionRunningTask(t, "t-long", "tc-long", nil)
	writeMailbox(t, "t-long", map[string]string{
		"status":  "success",
		"message": strings.Repeat("x", 40000),
	})
	expectTaskPhase(t, "t-long", kaalmv1beta1.TaskSucceeded)
	task := getTask(t, "t-long")
	if n := len(task.Status.AgentReportedMessage); n > maxAgentMessageBytes {
		t.Errorf("agentReportedMessage is %d bytes, want at most %d", n, maxAgentMessageBytes)
	}
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || len(c.Message) > maxAgentMessageBytes {
		t.Errorf("Completed condition missing or its message is unbounded: %v", c != nil)
	}
}

// expectTaskFailedBounded waits until the task is Failed and checks that its
// Completed condition has reason and a message that holds want and fits the
// condition schema's maxLength. A message past that limit fails the settle
// write, so the task would never leave Pending.
func expectTaskFailedBounded(t *testing.T, name, reason, want string) {
	t.Helper()
	expectTaskPhase(t, name, kaalmv1beta1.TaskFailed)
	c := condition(getTask(t, name).Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Fatalf("Completed = %+v, want False/%s", c, reason)
	}
	if len(c.Message) > conditionMessageLimit {
		t.Fatalf("Completed message is %d bytes, want at most %d", len(c.Message), conditionMessageLimit)
	}
	if !strings.Contains(c.Message, want) {
		t.Fatalf("Completed message does not hold %q", want)
	}
}

// A provider name has no length limit in the schema. The failure message
// quotes it bounded, so the task still settles Failed.
func TestTask_LongProviderNameStillFails(t *testing.T) {
	mkWorkloadClass(t, "tc-long-provname", nil) // empty allowedProviders => none allowed
	mkTask(t, "t-long-provname", "tc-long-provname", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: strings.Repeat("a", 40000)}},
		}
	})
	expectTaskFailedBounded(t, "t-long-provname",
		kaalmv1beta1.ReasonClassConstraintViolation, "(first 256 of 40000 bytes)")
}

// spec.image has no length limit in the schema. The rule 2 message quotes it
// bounded, so the task still settles Failed.
func TestTask_LongImageStillFails(t *testing.T) {
	image := "other.test/" + strings.Repeat("a", 40000) // outside registry.test/agents/*
	mkWorkloadClass(t, "tc-long-image", nil)
	mkTask(t, "t-long-image", "tc-long-image", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Image = image
	})
	expectTaskFailedBounded(t, "t-long-image", kaalmv1beta1.ReasonClassConstraintViolation,
		fmt.Sprintf("(first 256 of %d bytes) does not match", len(image)))
}

// expectTaskReadyMessage waits until the task's Ready condition has reason and
// a message that holds want.
func expectTaskReadyMessage(t *testing.T, name, reason, want string) {
	t.Helper()
	eventually(t, func() error {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
			return err
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != reason {
			return fmt.Errorf("Ready = %+v, want reason %s", c, reason)
		}
		if !strings.Contains(c.Message, want) {
			return fmt.Errorf("Ready message (%d bytes) does not hold %q", len(c.Message), want)
		}
		return nil
	})
}

// spec.agentClassRef.name has no length limit in the schema. The message
// quotes it bounded.
func TestTask_LongClassRefQuotedInPart(t *testing.T) {
	mkTask(t, "t-long-classref", strings.Repeat("c", 40000), nil)
	expectTaskReadyMessage(t, "t-long-classref", kaalmv1beta1.ReasonInvalidReference,
		"AgentClass \"")
	expectTaskReadyMessage(t, "t-long-classref", kaalmv1beta1.ReasonInvalidReference,
		"(first 256 of 40000 bytes) does not exist")
}

func TestTask_LongImagePullSecretQuotedInPart(t *testing.T) {
	mkWorkloadClass(t, "tc-long-pull", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: strings.Repeat("s", 40000)}}
	})
	mkTask(t, "t-long-pull", "tc-long-pull", nil)
	expectTaskReadyMessage(t, "t-long-pull", kaalmv1beta1.ReasonImagePullSecretMissing,
		"(first 256 of 40000 bytes) missing")
}

// The kubelet's waiting message has no length limit, and for InvalidImageName
// it names the image. The settled message copies it cut at the Event note
// limit.
func TestTask_LongInvalidImageMessageCut(t *testing.T) {
	mkWorkloadClass(t, "tc-long-badimg", nil)
	mkTask(t, "t-long-badimg", "tc-long-badimg", nil)
	eventually(t, func() error { return markCertReadyErr("t-long-badimg") })
	eventually(t, func() error {
		if taskPod(t, "t-long-badimg") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := taskPod(t, "t-long-badimg")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "agent",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "InvalidImageName", Message: strings.Repeat("m", 40000),
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-long-badimg", kaalmv1beta1.TaskFailed)
	c := condition(getTask(t, "t-long-badimg").Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || len(c.Message) > eventrec.MaxNoteBytes || !strings.HasSuffix(c.Message, "... (truncated)") {
		t.Fatalf("Completed = %+v, want a message of at most %d bytes ending with the truncation marker",
			c != nil, eventrec.MaxNoteBytes)
	}
}
