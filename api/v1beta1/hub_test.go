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

package v1beta1

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// TestHubMarkers proves every root type is a conversion hub. The marker
// methods carry no behavior; the test pins the set so a seventh kind cannot
// be added without deciding its place in the hub-and-spoke model.
func TestHubMarkers(t *testing.T) {
	hubs := []conversion.Hub{&Agent{}, &AgentChannel{}, &AgentClass{}, &AgentTask{}, &ModelProvider{}, &ToolProvider{}}
	if len(hubs) != 6 {
		t.Fatalf("expected six hubs, have %d", len(hubs))
	}
	for _, h := range hubs {
		h.Hub()
	}
}
