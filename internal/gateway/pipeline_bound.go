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

package gateway

import (
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// wakePollInterval is the wait between readiness connects after
// wakePollFastWindow, and the longest wait between connects in a Pod wait.
const wakePollInterval = 2 * time.Second

// wakePollFastInterval is the wait between readiness connects for the first
// wakePollFastWindow of a Pod wait. A woken Pod is usually reachable within a
// few seconds, so the wait checks often at first and delivers soon after the
// Pod is Ready.
const wakePollFastInterval = 250 * time.Millisecond

// wakePollFastWindow is how long a Pod wait polls every wakePollFastInterval.
// A wait that runs longer is usually an image pull or scheduling, where a
// check every wakePollInterval adds little delay.
const wakePollFastWindow = 5 * time.Second

// wakeConnectTimeout bounds each readiness connect of a Pod wait. Until the
// Pod is Ready, kube-proxy answers a connect to the empty Service with an
// ICMP port unreachable that the node rate-limits (net.ipv4.icmp_ratelimit,
// burst 6), so a SYN past the burst gets no answer. A hung connect must cost
// no more than a poll sleep, or a wake pays a full AgentConnectTimeout after
// the Pod turns Ready.
const wakeConnectTimeout = wakePollFastInterval

// wakePollDelay is the wait before the next readiness connect, elapsed into
// a Pod wait.
func wakePollDelay(elapsed time.Duration) time.Duration {
	if elapsed < wakePollFastWindow {
		return wakePollFastInterval
	}
	return wakePollInterval
}

// wakeConfirmQuickInterval is the wait between the first
// wakeConfirmQuickTries connects after the Agent's Pod turns Ready. The
// connect made at the Ready event is usually refused, because the Service
// routes a few to a few hundred milliseconds after Ready, so short steps
// deliver soon after it routes.
const wakeConfirmQuickInterval = 50 * time.Millisecond

// wakeConfirmQuickTries is how many connects after Ready run
// wakeConfirmQuickInterval apart. Each refused connect spends one of the
// node's rate-limited ICMP refusals (burst 6), so for one wait alone six
// quick tries fit that burst. The kernel keys the limit on the gateway Pod
// IP, so concurrent wakes through one gateway, and fallback polling earlier
// in the same wait, share it: a quick connect can still get no answer, and
// wakeConnectTimeout bounds it.
const wakeConfirmQuickTries = 6

// wakeConfirmDelay is the wait before the next readiness connect after the
// Pod turns Ready, with tries connects made since Ready and sinceReady
// elapsed since it.
func wakeConfirmDelay(tries int, sinceReady time.Duration) time.Duration {
	if tries < wakeConfirmQuickTries {
		return wakeConfirmQuickInterval
	}
	return wakePollDelay(sinceReady)
}

// activatorWorstCase is the longest the activator call can take: one attempt
// and its retry on a fresh connection, each bounded by activatorAttemptTimeout.
const activatorWorstCase = 2 * activatorAttemptTimeout

// pipelineMargin covers the pipeline steps no schedule bounds: Secret reads,
// DNS lookups before each callback attempt, and span and metric work.
const pipelineMargin = 30 * time.Second

// asyncResponseRequests is how many callback-schedule runs an async webhook
// pipeline makes after delivery: the callback, then the polling-record patch,
// which runs on the same schedule.
const asyncResponseRequests = 2

// retryScheduleWorstCase is the longest one bounded retry schedule takes:
// every attempt runs to its read timeout, and every wait between attempts
// elapses.
func retryScheduleWorstCase(backoff []time.Duration, readTimeout time.Duration) time.Duration {
	total := time.Duration(len(backoff)+1) * readTimeout
	for _, delay := range backoff {
		total += delay
	}
	return total
}

// backgroundPipelineBound is the deadline for one background pipeline run,
// derived from the budgets the run contains. The wake leg uses the same
// capped wakeTimeout as the wake wait, so the bound never cuts the wait
// short, and the gateway cap is what keeps one run from holding a goroutine
// for as long as a tenant sets. It sums:
//
//   - the activator call and the capped wakeTimeout, plus one readiness
//     check, which can be in flight when the deadline passes. A check, the
//     Service lookup and the connect together, is bounded by
//     gateway.agentDeliveryConnectTimeout (the connect alone also by
//     wakeConnectTimeout). This leg
//     covers the one Pod wait any message gets: a wake, a wait for a Pod
//     being created or resumed, or a wait after a refused connect, which
//     takes the place of a retry delay. The activator call is the one call a
//     message makes, before its Pod wait or during it; the wait keeps one
//     deadline, so a wake requested inside it stays inside this leg;
//   - the agent-delivery schedule, each attempt at gateway.agentReadTimeout;
//   - responseRequests runs of the callback schedule, each attempt at
//     gateway.callbackReadTimeout: the callback and the polling-record patch
//     for an async webhook, one per platform API request for a platform
//     reply;
//   - pipelineMargin.
//
// A nil agent (a platform message whose Agent no longer exists) has no wake
// or delivery leg.
func (s *Server) backgroundPipelineBound(agent *kaalmv1beta1.Agent, responseRequests int) time.Duration {
	bound := pipelineMargin +
		time.Duration(responseRequests)*retryScheduleWorstCase(s.Config.CallbackBackoff, s.callbackReadTimeout())
	if agent != nil {
		bound += activatorWorstCase + s.wakeTimeout(agent) + s.Config.AgentConnectTimeout +
			retryScheduleWorstCase(s.Config.DeliveryBackoff, s.Config.AgentReadTimeout)
	}
	return bound
}
