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
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// phaseCollectTimeout bounds the three cache reads behind one scrape so a
// cache that is still syncing cannot stall the metrics endpoint.
const phaseCollectTimeout = 5 * time.Second

// labelPhase is the phase label the three gauges share.
const labelPhase = "phase"

// The phase-count gauges (docs/src/controller/operations.md#observability).
// No _total suffix: OpenMetrics reserves it for counters.
var (
	agentsDesc = prometheus.NewDesc("kaalm_agents",
		"Agents by phase and namespace.", []string{labelPhase, labelNamespace}, nil)
	tasksDesc = prometheus.NewDesc("kaalm_tasks",
		"AgentTasks by phase and namespace.", []string{labelPhase, labelNamespace}, nil)
	channelsDesc = prometheus.NewDesc("kaalm_channels",
		"AgentChannels by namespace, phase, and the Ready and PlatformConnected conditions.",
		[]string{labelNamespace, labelPhase, "ready", "platform_connected"}, nil)
)

// PhaseCollector computes the three phase-count gauges from the manager cache
// on every scrape. Counting at scrape time rather than on reconcile means a
// namespace that empties simply stops appearing: no stale series to zero, no
// per-phase bookkeeping in the reconcilers. Every controller replica serves
// the gauges from its own cache, so dashboards aggregate them with max, not
// sum.
type PhaseCollector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c *PhaseCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- agentsDesc
	ch <- tasksDesc
	ch <- channelsDesc
}

// Collect implements prometheus.Collector. A failed list (cache not yet
// synced, API unavailable) contributes nothing for that kind; the other two
// still report.
func (c *PhaseCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), phaseCollectTimeout)
	defer cancel()

	var agents kaalmv1alpha1.AgentList
	if err := c.Reader.List(ctx, &agents); err == nil {
		counts := map[[2]string]float64{}
		for i := range agents.Items {
			a := &agents.Items[i]
			counts[[2]string{phaseOrPending(string(a.Status.Phase)), a.Namespace}]++
		}
		for k, v := range counts {
			ch <- prometheus.MustNewConstMetric(agentsDesc, prometheus.GaugeValue, v, k[0], k[1])
		}
	}

	var tasks kaalmv1alpha1.AgentTaskList
	if err := c.Reader.List(ctx, &tasks); err == nil {
		counts := map[[2]string]float64{}
		for i := range tasks.Items {
			t := &tasks.Items[i]
			counts[[2]string{phaseOrPending(string(t.Status.Phase)), t.Namespace}]++
		}
		for k, v := range counts {
			ch <- prometheus.MustNewConstMetric(tasksDesc, prometheus.GaugeValue, v, k[0], k[1])
		}
	}

	var channels kaalmv1alpha1.AgentChannelList
	if err := c.Reader.List(ctx, &channels); err == nil {
		counts := map[[4]string]float64{}
		for i := range channels.Items {
			c := &channels.Items[i]
			counts[[4]string{
				c.Namespace,
				phaseOrPending(string(c.Status.Phase)),
				conditionLabel(c.Status.Conditions, "Ready"),
				conditionLabel(c.Status.Conditions, "PlatformConnected"),
			}]++
		}
		for k, v := range counts {
			ch <- prometheus.MustNewConstMetric(channelsDesc, prometheus.GaugeValue, v, k[0], k[1], k[2], k[3])
		}
	}
}

// phaseOrPending names a resource the reconciler has not stamped yet. The
// empty string is not a usable label value, and Pending is where every Kaalm
// state machine starts.
func phaseOrPending(phase string) string {
	if phase == "" {
		return "Pending"
	}
	return phase
}

// conditionLabel lowers a condition's tri-state to the documented label
// vocabulary: true | false | unknown. A condition never set reads unknown.
func conditionLabel(conds []metav1.Condition, condType string) string {
	if c := meta.FindStatusCondition(conds, condType); c != nil {
		return strings.ToLower(string(c.Status))
	}
	return "unknown"
}
