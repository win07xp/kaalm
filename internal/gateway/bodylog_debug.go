//go:build kaalm_debug_logs

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

import "log/slog"

// This is the DEBUG build, gated by the `kaalm_debug_logs` build tag. It logs
// prompt and response bodies for contract bring-up and integration debugging.
// The official Helm chart only ships default builds; debug images carry a
// `-debug` tag suffix and emit a startup banner (see init). Never ship this to
// production. See docs/src/operations/observability.md.
const DebugBodyLogging = true

func init() {
	slog.Warn("KAALM DEBUG BUILD: prompt and response bodies WILL be logged. " +
		"Do not run this image in production.")
}

// bodyLog logs a body verbatim under the debug tag only.
func bodyLog(label string, body []byte) {
	slog.Debug("body", "label", label, "body", string(body))
}
