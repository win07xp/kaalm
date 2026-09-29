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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// With no effective idle timeout the Agent carries IdleDetection=False,
// reason Disabled, naming its class; a nonzero timeout removes it.
func TestSetIdleDetection(t *testing.T) {
	agent := &kaalmv1beta1.Agent{}
	setIdleDetection(agent, "wc-none", 0)
	c := condition(agent.Status.Conditions, kaalmv1beta1.ConditionIdleDetection)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonIdleDetectionDisabled {
		t.Fatalf("IdleDetection = %+v, want False/Disabled", c)
	}
	if !strings.Contains(c.Message, `"wc-none"`) {
		t.Errorf("message %q must name the AgentClass", c.Message)
	}

	setIdleDetection(agent, "wc-none", time.Minute)
	if c := condition(agent.Status.Conditions, kaalmv1beta1.ConditionIdleDetection); c != nil {
		t.Errorf("a nonzero idle timeout must remove the condition, got %+v", c)
	}
}

// End to end: an Agent whose class sets no idle timeout reports the
// condition; setting one on the Agent clears it.
func TestAgent_IdleDetectionCondition(t *testing.T) {
	mkWorkloadClass(t, "wc-noidle", nil)
	mkWorkloadAgent(t, "noidle", "wc-noidle", nil)
	eventually(t, func() error {
		c := condition(getWorkloadAgent(t, "noidle").Status.Conditions, kaalmv1beta1.ConditionIdleDetection)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("IdleDetection=False not set yet")
		}
		return nil
	})

	eventually(t, func() error {
		ag := getWorkloadAgent(t, "noidle")
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
		return testClient.Update(ctxT(), ag)
	})
	eventually(t, func() error {
		if c := condition(getWorkloadAgent(t, "noidle").Status.Conditions,
			kaalmv1beta1.ConditionIdleDetection); c != nil {
			return errString("IdleDetection still present after a nonzero idle timeout")
		}
		return nil
	})
}
