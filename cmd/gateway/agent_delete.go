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
	toolscache "k8s.io/client-go/tools/cache"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// agentDeleteHandler drops a deleted Agent's activity record from this
// replica's store, so the store does not keep it until the replica
// restarts. A relist that missed the delete hands over a tombstone, which
// the handler unwraps.
func agentDeleteHandler(activity *gateway.ActivityStore) toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			agent, ok := obj.(*kaalmv1beta1.Agent)
			if !ok {
				return
			}
			activity.Forget(agent.Namespace, agent.Name)
		},
	}
}
