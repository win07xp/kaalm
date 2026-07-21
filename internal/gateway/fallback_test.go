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
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestIsFallbackable(t *testing.T) {
	cases := []struct {
		status int
		fall   bool
		class  failClass
	}{
		{200, false, classNone},
		{500, true, classUpstream},
		{503, true, classUpstream},
		{429, true, classUpstream},
		{401, true, classUpstream},
		{403, true, classUpstream},
		{400, false, classNonFallle},
		{422, false, classNonFallle},
		{404, false, classNonFallle},
	}
	for _, c := range cases {
		fall, class := isFallbackable(c.status)
		if fall != c.fall || class != c.class {
			t.Errorf("isFallbackable(%d) = (%v,%v), want (%v,%v)", c.status, fall, class, c.fall, c.class)
		}
	}
}

// fallbackHarness builds a server with a fake store of providers and an
// attempt function whose per-provider outcome is scripted.
type fallbackHarness struct {
	server  *Server
	store   *fakeStore
	results map[string]int // provider name -> HTTP status to return (0 = connect error)
}

func newFallbackHarness() *fallbackHarness {
	h := &fallbackHarness{store: newFakeStore(), results: map[string]int{}}
	h.server = &Server{
		Store:  h.store,
		Budget: NewBudgetLedger(),
		Config: Config{MaxFallbackDepth: 3},
	}
	return h
}

func (h *fallbackHarness) provider(name, ptype string, fallbacks ...string) *agentryv1alpha1.ModelProvider {
	p := &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agentryv1alpha1.ModelProviderSpec{
			Type:   ptype,
			Models: []agentryv1alpha1.ModelProviderModel{{ID: "m1"}},
		},
	}
	for _, f := range fallbacks {
		p.Spec.Fallback = append(p.Spec.Fallback, agentryv1alpha1.LocalObjectReference{Name: f})
	}
	h.store.providers[name] = p
	return p
}

// run walks from primary and returns the chosen provider name (or "" on
// exhaustion) plus the total attemptCount budget consumed (which counts
// budget-blocked candidates that never forward).
func (h *fallbackHarness) run(primary *agentryv1alpha1.ModelProvider) (string, int, bool) {
	st := &walkState{primary: primary, namespace: "team-a", modelID: "m1", maxDepth: 3, visited: map[string]bool{}}
	res, ok := h.server.tryWithFallbacks(context.Background(), primary, st,
		func(_ context.Context, cand *agentryv1alpha1.ModelProvider) forwardResult {
			status := h.results[cand.Name]
			if status == 0 {
				return forwardResult{fallilable: true, class: classConnect, provider: cand.Name}
			}
			fall, class := isFallbackable(status)
			return forwardResult{
				resp:       &http.Response{StatusCode: status, Body: http.NoBody},
				fallilable: fall, class: class, provider: cand.Name, chosen: cand,
			}
		})
	if !ok {
		return "", st.attemptCount, false
	}
	return res.provider, st.attemptCount, true
}

func TestFallback_PrimarySucceeds(t *testing.T) {
	h := newFallbackHarness()
	p := h.provider("primary", "anthropic", "backup")
	h.provider("backup", "anthropic")
	h.results["primary"] = 200
	chosen, attempts, ok := h.run(p)
	if !ok || chosen != "primary" || attempts != 1 {
		t.Errorf("primary success: chosen=%q attempts=%d ok=%v", chosen, attempts, ok)
	}
}

func TestFallback_WalksToBackup(t *testing.T) {
	h := newFallbackHarness()
	p := h.provider("primary", "anthropic", "backup", "dr")
	h.provider("backup", "anthropic")
	h.provider("dr", "anthropic")
	h.results["primary"] = 503 // fallbackable
	h.results["backup"] = 200
	chosen, attempts, ok := h.run(p)
	if !ok || chosen != "backup" || attempts != 2 {
		t.Errorf("walk to backup: chosen=%q attempts=%d ok=%v", chosen, attempts, ok)
	}
}

func TestFallback_DepthCap(t *testing.T) {
	h := newFallbackHarness()
	// A deep chain; the cap of 3 stops after primary + 2.
	p := h.provider("p0", "anthropic", "p1")
	h.provider("p1", "anthropic", "p2").Spec.Fallback = []agentryv1alpha1.LocalObjectReference{{Name: "p2"}}
	h.provider("p2", "anthropic", "p3")
	h.provider("p3", "anthropic")
	for _, n := range []string{"p0", "p1", "p2", "p3"} {
		h.results[n] = 503
	}
	_, attempts, ok := h.run(p)
	if ok || attempts != 3 {
		t.Errorf("depth cap: attempts=%d ok=%v, want 3 attempts and exhaustion", attempts, ok)
	}
}

func TestFallback_NonFallbackablePassesThrough(t *testing.T) {
	h := newFallbackHarness()
	p := h.provider("primary", "anthropic", "backup")
	h.provider("backup", "anthropic")
	h.results["primary"] = 400 // malformed: do NOT fall back
	h.results["backup"] = 200
	chosen, attempts, ok := h.run(p)
	if !ok || chosen != "primary" || attempts != 1 {
		t.Errorf("400 must pass through: chosen=%q attempts=%d ok=%v", chosen, attempts, ok)
	}
}

func TestFallback_StaticIneligibleNoSlot(t *testing.T) {
	h := newFallbackHarness()
	p := h.provider("primary", "anthropic", "wrong-type", "good")
	// wrong-type is skipped without a slot; good then succeeds.
	h.provider("wrong-type", "openai")
	h.provider("good", "anthropic")
	h.results["primary"] = 503
	h.results["good"] = 200
	chosen, attempts, ok := h.run(p)
	// primary (1) + good (2); wrong-type consumes no slot.
	if !ok || chosen != "good" || attempts != 2 {
		t.Errorf("static skip: chosen=%q attempts=%d ok=%v", chosen, attempts, ok)
	}
}

func TestFallback_CycleDefense(t *testing.T) {
	h := newFallbackHarness()
	// primary -> backup -> primary (cycle); runtime visited stops it.
	p := h.provider("primary", "anthropic", "backup")
	h.provider("backup", "anthropic", "primary")
	h.results["primary"] = 503
	h.results["backup"] = 503
	_, _, ok := h.run(p)
	if ok {
		t.Error("a cycle with all-failing providers must exhaust, not loop")
	}
}

func TestFallback_BudgetBlockedPrimaryHandledByCaller(t *testing.T) {
	// The handler short-circuits a budget-blocked primary to 429 before the
	// walk; here we assert a budget-blocked candidate mid-walk consumes a
	// slot and falls through to its children.
	h := newFallbackHarness()
	p := h.provider("primary", "anthropic", "blocked")
	blocked := h.provider("blocked", "anthropic", "child")
	blocked.Spec.Budget = agentryv1alpha1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "10",
		Policies: []agentryv1alpha1.ModelProviderBudgetPolicy{{AtPercent: 100, Action: "block"}},
	}
	h.server.Budget.Add(blocked, "team-a", 20) // 200% -> blocked
	h.provider("child", "anthropic")
	h.results["primary"] = 503
	h.results["child"] = 200
	chosen, attempts, ok := h.run(p)
	// primary (1) + blocked-slot (2) + child (3).
	if !ok || chosen != "child" || attempts != 3 {
		t.Errorf("budget-blocked mid-walk: chosen=%q attempts=%d ok=%v", chosen, attempts, ok)
	}
}

func TestExhaustionErrorMapping(t *testing.T) {
	if st, _ := exhaustionError(map[failClass]bool{classConnect: true}, "p"); st != http.StatusServiceUnavailable {
		t.Errorf("all-connect = %d, want 503", st)
	}
	if st, _ := exhaustionError(map[failClass]bool{classTimeout: true}, "p"); st != http.StatusGatewayTimeout {
		t.Errorf("all-timeout = %d, want 504", st)
	}
	if st, _ := exhaustionError(map[failClass]bool{classUpstream: true, classConnect: true}, "p"); st != http.StatusBadGateway {
		t.Errorf("mixed = %d, want 502", st)
	}
}

func TestFailClassName(t *testing.T) {
	cases := map[failClass]string{
		classConnect:   "connect_error",
		classTimeout:   "timeout",
		classBudget:    "budget_blocked",
		classUpstream:  "upstream_error",
		classNonFallle: "other",
	}
	for c, want := range cases {
		if got := failClassName(c); got != want {
			t.Errorf("failClassName(%d) = %q, want %q", c, got, want)
		}
	}
}
