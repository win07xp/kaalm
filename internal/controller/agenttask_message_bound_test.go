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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The gateway accepts a completion body of up to 1 MiB, and a condition
// message is capped at 32768 characters by its schema. A long agent message
// must still let the task settle, with the stored copies bounded.
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
