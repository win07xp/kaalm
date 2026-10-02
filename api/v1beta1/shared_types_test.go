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

import "testing"

// Rule 48: only the exact value "true" of the workload label opts a Secret in
// to workload env use. The channel label does not count.
func TestWorkloadSecretOptedIn(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{"kaalm.io/workload-secret": "true"}, true},
		{map[string]string{"kaalm.io/workload-secret": "True"}, false},
		{map[string]string{"kaalm.io/workload-secret": "yes"}, false},
		{map[string]string{"kaalm.io/workload-secret": ""}, false},
		{map[string]string{"kaalm.io/workload-secrets": "true"}, false},
		{map[string]string{"kaalm.io/channel-credential": "true"}, false},
	}
	for _, tc := range cases {
		if got := WorkloadSecretOptedIn(tc.labels); got != tc.want {
			t.Errorf("WorkloadSecretOptedIn(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}
