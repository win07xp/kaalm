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

// wakePollInterval is the wait between readiness connects during a wake.
const wakePollInterval = 2 * time.Second

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
// derived from the budgets the run contains, so no Agent's wakeTimeout is cut
// short by a fixed ceiling. It sums:
//
//   - the activator call and the Agent's effective wakeTimeout, plus one
//     readiness connect and poll interval, which the last poll can overrun;
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
		bound += activatorWorstCase + s.wakeTimeout(agent) + s.Config.AgentConnectTimeout + wakePollInterval +
			retryScheduleWorstCase(s.Config.DeliveryBackoff, s.Config.AgentReadTimeout)
	}
	return bound
}
