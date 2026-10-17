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
