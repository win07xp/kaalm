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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// setIdleDetection reports an effective idle timeout of zero, which turns
// the activity check, and with it the idle and hibernation cycle, off:
// IdleDetection=False, reason Disabled, naming the class. Once a nonzero
// timeout applies the condition is removed rather than set True, so the
// common case carries no extra condition. No event: the condition follows
// the spec, and the spec change is the event.
func setIdleDetection(agent *kaalmv1beta1.Agent, className string, idleTimeout time.Duration) {
	if idleTimeout > 0 {
		apimeta.RemoveStatusCondition(&agent.Status.Conditions, kaalmv1beta1.ConditionIdleDetection)
		return
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:   kaalmv1beta1.ConditionIdleDetection,
		Status: metav1.ConditionFalse,
		Reason: kaalmv1beta1.ReasonIdleDetectionDisabled,
		Message: fmt.Sprintf("idle detection is off: neither the Agent nor its AgentClass %q sets an idle timeout",
			className),
	})
}
