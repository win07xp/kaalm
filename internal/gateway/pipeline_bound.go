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

// wakePollDelay is the wait before the next readiness connect, elapsed into
// a Pod wait.
func wakePollDelay(elapsed time.Duration) time.Duration {
	if elapsed < wakePollFastWindow {
		return wakePollFastInterval
	}
	return wakePollInterval
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
//     connect, which can be in flight when the deadline passes. This leg
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
