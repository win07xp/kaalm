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
	"math/rand/v2"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The requeue of a Running or Idle Agent while no gateway replica answers
// the activity fan-out: 30 seconds at first, doubling to five minutes, with
// up to 10 percent jitter either way.
const (
	gatewayOutageBaseRequeue = 30 * time.Second
	gatewayOutageMaxRequeue  = 5 * time.Minute
	gatewayOutageJitter      = 0.1
)

// outageJitter draws the jitter in [0, 1); tests replace it.
var outageJitter = rand.Float64 //nolint:gosec // requeue spread, not a secret

// gatewayOutageRequeue returns the delay before an Agent waiting on the
// gateway re-checks it. While GatewayReachable is False, the delay is the
// base plus the time since the condition went False, so each pass doubles
// the wait (30s, 1m, 2m, 4m), capped at five minutes, the same shape as
// probeRequeue. The outage start is read from the condition's
// lastTransitionTime rather than kept in memory, so a controller restart
// keeps the backoff. u in [0, 1) scales the delay by 0.9 to 1.1, so Agents
// that lost the gateway together spread out; the cap still holds. The
// recovery kick (agentsWaitingOnGateway) keeps the long delays from slowing
// recovery.
func gatewayOutageRequeue(conds []metav1.Condition, now time.Time, u float64) time.Duration {
	d := gatewayOutageBaseRequeue
	if c := apimeta.FindStatusCondition(conds, kaalmv1beta1.ConditionGatewayReachable); c != nil &&
		c.Status == metav1.ConditionFalse {
		if down := now.Sub(c.LastTransitionTime.Time); down > 0 {
			d = min(d+down, gatewayOutageMaxRequeue)
		}
	}
	factor := 1 - gatewayOutageJitter + 2*gatewayOutageJitter*u
	return min(time.Duration(float64(d)*factor), gatewayOutageMaxRequeue)
}

// gatewayTurnedReady admits the gateway Pod events the recovery kick acts
// on: gatewayReadinessChanged without deletes. A deleted Pod never brings
// the gateway back, and its last-seen state can still read Ready.
func gatewayTurnedReady(operatorNamespace string) predicate.Predicate {
	return predicate.And(gatewayReadinessChanged(operatorNamespace), predicate.Funcs{
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	})
}

// unreachableForgetter is the optional ActivityClient hook the recovery
// kick calls: drop cached answers that carried no activity data, so the
// kicked passes dial the gateway instead of reading a stale "no data".
type unreachableForgetter interface {
	ForgetUnreachable()
}

// activityStepRuns reports whether the activity step evaluates the Agent:
// it is Running or Idle, its effective idleTimeout is above zero, and the
// controller has an activity client.
func (r *AgentReconciler) activityStepRuns(agent *kaalmv1beta1.Agent, idleTimeout time.Duration) bool {
	return r.Activity != nil && idleTimeout > 0 &&
		(agent.Status.Phase == kaalmv1beta1.AgentRunning || agent.Status.Phase == kaalmv1beta1.AgentIdle)
}

// dropGatewayReachableUnlessEvaluated removes GatewayReachable from an Agent
// the activity step does not evaluate. The condition reports the step's
// last fan-out, so without the step a value would only go stale, and a
// stale False would keep the Agent on the recovery kick's list.
func (r *AgentReconciler) dropGatewayReachableUnlessEvaluated(agent *kaalmv1beta1.Agent, idleTimeout time.Duration) {
	if !r.activityStepRuns(agent, idleTimeout) {
		apimeta.RemoveStatusCondition(&agent.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
	}
}

// agentsWaitingOnGateway is the recovery kick: when a gateway Pod turns
// Ready, it enqueues every Running or Idle Agent whose GatewayReachable is
// False, so the outage backoff never delays recovery. It skips the Agents
// the activity step does not evaluate: all of them when the controller has
// no activity client, and those with idle detection off (IdleDetection
// False, the status mark of a zero effective idleTimeout). It lists Agents from
// the cache without a field index and without deep copies: gateway Pod
// readiness changes are rare (a rollout, a restart), and one list per
// change stays cheap at a thousand Agents, where an index on a status
// condition would be rebuilt on every Agent status write. Each gateway Pod
// event enqueues each waiting Agent once, and the workqueue folds the events
// of a rollout together. A Pod leaving Ready enqueues nothing: the Agents
// find the outage on their own next pass.
func (r *AgentReconciler) agentsWaitingOnGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	if r.Activity == nil || !countsAsReadyGateway(obj) {
		return nil
	}
	if f, ok := r.Activity.(unreachableForgetter); ok {
		f.ForgetUnreachable()
	}
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.UnsafeDisableDeepCopy); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range agents.Items {
		a := &agents.Items[i]
		if a.Status.Phase != kaalmv1beta1.AgentRunning && a.Status.Phase != kaalmv1beta1.AgentIdle {
			continue
		}
		if apimeta.IsStatusConditionFalse(a.Status.Conditions, kaalmv1beta1.ConditionIdleDetection) {
			continue
		}
		if apimeta.IsStatusConditionFalse(a.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		}
	}
	return reqs
}
