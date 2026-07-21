/*
Copyright 2026.

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

import "testing"

// TestDefaultBuildLogsNoBodies asserts the default build's PII posture: body
// logging is compiled out. The debug build overrides this only under the
// agentry_debug_logs tag.
func TestDefaultBuildLogsNoBodies(t *testing.T) {
	if DebugBodyLogging {
		t.Fatal("the default test build must have body logging disabled; " +
			"do not run tests with -tags agentry_debug_logs in CI")
	}
	// bodyLog must be a safe no-op in the default build.
	bodyLog("prompt", []byte("secret prompt content"))
}

func TestBodyLogIsNoOp(t *testing.T) {
	// Default build: bodyLog must not panic and logs nothing.
	bodyLog("prompt", []byte("secret content"))
	if DebugBodyLogging {
		t.Error("default build must have body logging disabled")
	}
}
