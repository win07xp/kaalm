//go:build !kaalm_debug_logs

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

// This is the DEFAULT build. bodyLog is a no-op: in the default build, prompt
// and response bodies are never logged at any level, on any code path. The
// gateway logs request metadata only (namespace, workload, model, status,
// latency, token counts). See docs/src/operations/observability.md.
//
// There is no runtime flag, environment variable, or admin endpoint that flips
// body logging on. The only way to log bodies is to compile with the
// `kaalm_debug_logs` build tag (see bodylog_debug.go), which produces a
// separate image with a startup banner. This keeps the production wire format
// provably PII-clean.
const DebugBodyLogging = false

// bodyLog is compiled out of the default build. Call sites pass body bytes;
// the default implementation ignores them entirely.
func bodyLog(_ string, _ []byte) {}
