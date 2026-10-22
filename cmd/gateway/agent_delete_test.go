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

package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

func activityWithAgent(t *testing.T) *gateway.ActivityStore {
	t.Helper()
	activity := gateway.NewActivityStore()
	activity.RecordTraffic("team", "sup")
	return activity
}

func hasRecord(activity *gateway.ActivityStore) bool {
	_, ok := activity.Snapshot("team").Agents["sup"]
	return ok
}

func deletedAgent() *kaalmv1beta1.Agent {
	return &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "sup"}}
}

func TestAgentDeleteHandler_ForgetsTheAgent(t *testing.T) {
	activity := activityWithAgent(t)
	agentDeleteHandler(activity).OnDelete(deletedAgent())
	if hasRecord(activity) {
		t.Error("a deleted Agent's activity record must be dropped")
	}
}

// A relist that missed the delete hands the handler a tombstone.
func TestAgentDeleteHandler_UnwrapsTombstone(t *testing.T) {
	activity := activityWithAgent(t)
	agentDeleteHandler(activity).OnDelete(toolscache.DeletedFinalStateUnknown{Key: "team/sup", Obj: deletedAgent()})
	if hasRecord(activity) {
		t.Error("a tombstoned Agent's activity record must be dropped")
	}
}

func TestAgentDeleteHandler_IgnoresOtherKinds(t *testing.T) {
	activity := activityWithAgent(t)
	channel := &kaalmv1beta1.AgentChannel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "sup"}}
	agentDeleteHandler(activity).OnDelete(channel)
	if !hasRecord(activity) {
		t.Error("a delete of another kind must leave the Agent's record")
	}
}
