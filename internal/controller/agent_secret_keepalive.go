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
	"math/rand/v2"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// The Secret watcher stops an informer nobody has read for its IdleTTL (one
// hour by default). An Agent with no sooner requeue would stop seeing changes
// to its pull and env Secrets an hour after its last pass. A pass about every
// half hour reads them again, which keeps the watches alive, so a Secret
// change still re-runs the Agent at once. The jitter, 10 percent either way,
// spreads out a fleet of Agents created together.
const (
	secretKeepAliveRequeue = 30 * time.Minute
	secretKeepAliveJitter  = 0.1
)

// secretKeepAliveDraw draws the jitter in [0, 1).
var secretKeepAliveDraw = rand.Float64 //nolint:gosec // requeue spread, not a secret

// secretKeepAlive returns the keep-alive requeue for u in [0, 1]: 27 to 33
// minutes.
func secretKeepAlive(u float64) time.Duration {
	factor := 1 - secretKeepAliveJitter + 2*secretKeepAliveJitter*u
	return time.Duration(float64(secretKeepAliveRequeue) * factor)
}

// withSecretKeepAlive makes sure a pass that read Secrets is retried within
// the keep-alive requeue, keeping any sooner requeue.
func withSecretKeepAlive(res ctrl.Result, u float64) ctrl.Result {
	if d := secretKeepAlive(u); res.RequeueAfter == 0 || res.RequeueAfter > d {
		res.RequeueAfter = d
	}
	return res
}

// readsSecrets reports whether an Agent pass reads Secrets through the
// watcher: the class pull Secrets (rule 23) or the env Secrets (rule 48).
func readsSecrets(eff effectiveAgentSpec) bool {
	return len(eff.ImagePullSecrets) > 0 || len(envSecretRefs(eff.Env)) > 0
}
