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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// maxProbeBackoff is the absolute ceiling on a failing provider's probe
// delay; the cap is also never more than maxProbeBackoffIntervals intervals.
const (
	maxProbeBackoff          = 10 * time.Minute
	maxProbeBackoffIntervals = 10
)

// probeRequeue returns the delay before the next periodic liveness probe of
// a ModelProvider or ToolProvider. A provider whose Healthy condition is not
// False requeues at interval. While it is False, the delay is interval plus
// the time since the condition went False, so each periodic failure doubles
// the wait (interval, 2x, 4x, 8x, ...), capped at ten intervals or ten
// minutes, whichever is smaller, and never below interval. The failing time
// is read from the condition's lastTransitionTime rather than kept in
// memory, so a controller restart keeps the backoff; one success resets it.
func probeRequeue(conds []metav1.Condition, interval time.Duration, now time.Time) time.Duration {
	c := apimeta.FindStatusCondition(conds, kaalmv1beta1.ConditionHealthy)
	if c == nil || c.Status != metav1.ConditionFalse {
		return interval
	}
	failing := now.Sub(c.LastTransitionTime.Time)
	if failing <= 0 {
		return interval
	}
	ceiling := min(maxProbeBackoffIntervals*interval, maxProbeBackoff)
	ceiling = max(ceiling, interval)
	return min(interval+failing, ceiling)
}

// setHealthyNotProbed sets Healthy=Unknown with reason NotProbed on a
// ModelProvider or ToolProvider pass that ends without probing: a failing
// credential or configuration check, a disabled probe, or a held delete. True
// and False come only from the latest probe (see probeGate). Unknown, not False, is
// what lets probeRequeue start a fresh backoff at the next probe failure:
// time spent not probing says nothing about how long the upstream has been
// failing.
func setHealthyNotProbed(conds *[]metav1.Condition, why string) {
	apimeta.SetStatusCondition(conds, metav1.Condition{
		Type:    kaalmv1beta1.ConditionHealthy,
		Status:  metav1.ConditionUnknown,
		Reason:  kaalmv1beta1.ReasonNotProbed,
		Message: "the probe did not run: " + why,
	})
}
