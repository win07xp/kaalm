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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestRetryScheduleWorstCase(t *testing.T) {
	// Four attempts at 10s each plus the 1s, 5s, and 25s waits: the 71s the
	// book quotes for the default schedules.
	got := retryScheduleWorstCase([]time.Duration{time.Second, 5 * time.Second, 25 * time.Second}, 10*time.Second)
	if got != 71*time.Second {
		t.Errorf("default schedule worst case = %s, want 71s", got)
	}
	if got := retryScheduleWorstCase(nil, 3*time.Second); got != 3*time.Second {
		t.Errorf("no-retry schedule worst case = %s, want one attempt of 3s", got)
	}
}

func TestBackgroundPipelineBound_Arithmetic(t *testing.T) {
	s := &Server{Config: Config{
		AgentConnectTimeout: time.Second,
		AgentReadTimeout:    10 * time.Second,
		CallbackReadTimeout: 7 * time.Second,
		DeliveryBackoff:     []time.Duration{time.Second, 5 * time.Second, 25 * time.Second},
		CallbackBackoff:     []time.Duration{2 * time.Second, 3 * time.Second},
	}}
	agent := &kaalmv1beta1.Agent{}
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: time.Hour}
	// The controller's effective value wins over the spec, as it does for the
	// wake itself.
	agent.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: 20 * time.Minute}

	wake := activatorWorstCase + 20*time.Minute + time.Second + wakePollInterval
	delivery := 4*10*time.Second + 31*time.Second
	callback := 3*7*time.Second + 5*time.Second

	for _, responseRequests := range []int{0, 1, 2, 5} {
		want := wake + delivery + time.Duration(responseRequests)*callback + pipelineMargin
		if got := s.backgroundPipelineBound(agent, responseRequests); got != want {
			t.Errorf("bound with %d response requests = %s, want %s", responseRequests, got, want)
		}
	}

	// A platform message whose Agent no longer exists runs the reply half
	// only.
	if got, want := s.backgroundPipelineBound(nil, 1), callback+pipelineMargin; got != want {
		t.Errorf("bound without an Agent = %s, want %s", got, want)
	}
}

func TestBackgroundPipelineBound_ScalesWithWakeTimeout(t *testing.T) {
	s := &Server{Config: Config{AgentReadTimeout: time.Second}}
	short := &kaalmv1beta1.Agent{}
	long := &kaalmv1beta1.Agent{}
	long.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 30 * time.Minute}

	// The default 2-minute wake and a 30-minute wake differ by exactly the
	// extra 28 minutes: no fixed ceiling truncates the larger one.
	if diff := s.backgroundPipelineBound(long, 1) - s.backgroundPipelineBound(short, 1); diff != 28*time.Minute {
		t.Errorf("bound difference = %s, want 28m", diff)
	}
}
