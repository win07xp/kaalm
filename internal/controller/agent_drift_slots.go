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
	"context"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// defaultMaxUnavailableOnDrift applies when a class sets no
// lifecycle.maxUnavailableOnDrift. The schema default covers a class that
// has a lifecycle block; this covers one that has none.
var defaultMaxUnavailableOnDrift = intstr.FromString("25%")

// driftReservationTTL bounds how long an in-memory reservation counts
// against the cap without the cache showing the Agent as Replacing. The
// reconcile persists Replacing before it deletes the Pod, so the
// reservation only covers cache lag, or a status write that failed.
const driftReservationTTL = 30 * time.Second

// driftPendingRequeue is the fallback retry for an Agent waiting for a
// drift slot. The normal trigger is the Agent watch that fires when another
// Agent of the class leaves Replacing.
var driftPendingRequeue = 30 * time.Second

// maxUnavailableOnDrift resolves the class's cap against the number of
// Agents in the class: a percentage rounds up, and the result is never
// below 1, so a rollout always makes progress.
func maxUnavailableOnDrift(class *kaalmv1beta1.AgentClass, total int) int {
	limit := defaultMaxUnavailableOnDrift
	if class.Spec.Lifecycle.MaxUnavailableOnDrift != nil {
		limit = *class.Spec.Lifecycle.MaxUnavailableOnDrift
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&limit, total, true)
	if err != nil {
		// Rule 44 rejects such a value at admission; this guards objects
		// stored before the rule existed.
		n, _ = intstr.GetScaledValueFromIntOrPercent(&defaultMaxUnavailableOnDrift, total, true)
	}
	return max(n, 1)
}

// podUpToDateReason returns the reason of the Agent's PodUpToDate
// condition, or "" when it has none.
func podUpToDateReason(agent *kaalmv1beta1.Agent) string {
	if c := apimeta.FindStatusCondition(agent.Status.Conditions, kaalmv1beta1.ConditionPodUpToDate); c != nil {
		return c.Reason
	}
	return ""
}

// driftSlots hands out the per-class slots that bound concurrent spec-drift
// replacements (maxUnavailableOnDrift). The count comes from the cache: every
// Agent of the class whose PodUpToDate reason is Replacing holds a slot. The
// reservations cover the window in which a slot was granted but the cache
// does not yet show the Replacing condition, since several reconciles run at
// once. Only the leader reconciles, and after a restart the count is rebuilt
// from the conditions alone.
type driftSlots struct {
	mu       sync.Mutex
	reserved map[types.NamespacedName]time.Time
}

// acquire reports whether the Agent may replace its drifted Pod now. It
// refuses when the class's Replacing Agents and live reservations fill the
// cap, and it refuses a non-Idle Agent while an Idle Agent of the class waits
// for a slot, so idle Agents are replaced first. A grant reserves a slot for
// the Agent until the cache shows it as Replacing.
func (s *driftSlots) acquire(
	ctx context.Context, c client.Reader, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, now time.Time,
) (bool, error) {
	// The items are the cache's own objects, only read here.
	var agents kaalmv1beta1.AgentList
	if err := c.List(ctx, &agents, client.MatchingFields{IndexAgentClassRef: class.Name}, client.UnsafeDisableDeepCopy); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserved == nil {
		s.reserved = map[types.NamespacedName]time.Time{}
	}
	for key, at := range s.reserved {
		if now.Sub(at) >= driftReservationTTL {
			delete(s.reserved, key)
		}
	}
	self := client.ObjectKeyFromObject(agent)
	inUse, idleWaiting := 0, false
	for i := range agents.Items {
		other := &agents.Items[i]
		key := client.ObjectKeyFromObject(other)
		if key == self {
			continue
		}
		switch podUpToDateReason(other) {
		case kaalmv1beta1.ReasonReplacing:
			inUse++
			delete(s.reserved, key)
			continue
		case kaalmv1beta1.ReasonReplacementPending:
			if other.Status.Phase == kaalmv1beta1.AgentIdle {
				idleWaiting = true
			}
		}
		if _, ok := s.reserved[key]; ok {
			inUse++
		}
	}
	if inUse >= maxUnavailableOnDrift(class, len(agents.Items)) {
		return false, nil
	}
	if idleWaiting && agent.Status.Phase != kaalmv1beta1.AgentIdle {
		return false, nil
	}
	s.reserved[self] = now
	return true, nil
}
