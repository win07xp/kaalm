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

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func rlProvider(rpm int32) *agentryv1alpha1.ModelProvider {
	return &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov"},
		Spec:       agentryv1alpha1.ModelProviderSpec{RateLimits: agentryv1alpha1.ModelProviderRateLimits{RequestsPerMinute: rpm}},
	}
}

func TestRateLimiter_UnlimitedAllows(t *testing.T) {
	rl := NewRateLimiter(nil)
	p := rlProvider(0)
	for i := 0; i < 100; i++ {
		if !rl.Allow(p, "team-a", "m1") {
			t.Fatal("a provider with no limit must always allow")
		}
	}
}

func TestRateLimiter_EnforcesCeiling(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(5)

	// The bucket starts full at the per-replica ceiling (5).
	allowed := 0
	for i := 0; i < 10; i++ {
		if rl.Allow(p, "team-a", "m1") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("allowed %d of 10, want 5 (the ceiling)", allowed)
	}
	// A different (namespace, model) has its own bucket.
	if !rl.Allow(p, "team-b", "m1") {
		t.Error("a different namespace must have a fresh bucket")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	rl := NewRateLimiter(func() int { return 1 })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(60) // 1 token/sec

	for i := 0; i < 60; i++ {
		rl.Allow(p, "team-a", "m1")
	}
	if rl.Allow(p, "team-a", "m1") {
		t.Fatal("bucket should be empty after draining the ceiling")
	}
	// 2 seconds later, ~2 tokens have refilled.
	now = now.Add(2 * time.Second)
	if !rl.Allow(p, "team-a", "m1") {
		t.Error("first refilled token should be available")
	}
	if !rl.Allow(p, "team-a", "m1") {
		t.Error("second refilled token should be available")
	}
}

func TestRateLimiter_DividesByReplicas(t *testing.T) {
	replicas := 4
	rl := NewRateLimiter(func() int { return replicas })
	now := time.Now()
	rl.now = func() time.Time { return now }
	p := rlProvider(8) // cluster ceiling 8, per-replica 2

	allowed := 0
	for i := 0; i < 10; i++ {
		if rl.Allow(p, "team-a", "m1") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("with 4 replicas each gets 8/4=2, allowed %d", allowed)
	}
}
