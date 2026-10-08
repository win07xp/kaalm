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

import (
	"sync"
	"testing"
	"time"
)

func TestCachedCountEvaluatesOncePerWindow(t *testing.T) {
	calls := 0
	count := CachedCount(time.Hour, func() int { calls++; return calls })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = count() }()
	}
	wg.Wait()
	if calls != 1 || count() != 1 {
		t.Fatalf("calls=%d count=%d, want one evaluation shared by every caller", calls, count())
	}
}

func TestCachedCountReevaluatesAfterTTL(t *testing.T) {
	calls := 0
	count := CachedCount(10*time.Millisecond, func() int { calls++; return calls })
	if count() != 1 {
		t.Fatal("first call must evaluate")
	}
	time.Sleep(20 * time.Millisecond)
	if got := count(); got != 2 || calls != 2 {
		t.Fatalf("after the window: count=%d calls=%d, want a fresh evaluation", got, calls)
	}
}

// NewServer gives the rate limiter and the hard budget their own counters:
// the limiter's share leaves out draining replicas, the margin counts them.
func TestNewServer_WiresSeparateReplicaCounters(t *testing.T) {
	s := NewServer(Config{
		RateLimitReplicas: func() int { return 2 },
		BudgetReplicas:    func() int { return 4 },
	}, newFakeStore(), nil, nil)
	if got := s.RateLimiter.replicas(); got != 2 {
		t.Errorf("rate limiter replicas = %d, want 2", got)
	}
	if got := s.Budget.replicasCount(); got != 4 {
		t.Errorf("budget replicas = %d, want 4", got)
	}
}
