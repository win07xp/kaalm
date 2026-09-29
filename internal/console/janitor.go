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

package console

import (
	"context"
	"time"
)

// sweepInterval is how often the janitor sweeps the three in-memory caches:
// the shortest TTL among them, so no expired entry outlives its TTL by more
// than one interval.
const sweepInterval = sarCacheTTL

// janitor sweeps the session store, the gate cache, and the review cache
// every interval until ctx is cancelled. Without it an entry leaves memory
// only when the same key is looked up again after it expires.
func (s *Server) janitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Server) sweep() {
	if cr, ok := s.Reviewer.(*CachingReviewer); ok {
		cr.Sweep()
	}
	if s.Gate != nil {
		s.Gate.Sweep()
	}
	if s.Sessions != nil {
		s.Sessions.Sweep()
	}
}
