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
	"strings"
	"testing"
)

func TestSessionWrapRoundTrip(t *testing.T) {
	key := []byte("k1")
	agentID := callerIdentity(&caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}})

	wrapped := wrapSessionID(key, "upstream-123", agentID)
	if strings.Contains(wrapped, "upstream-123") {
		t.Fatal("wrapped id must not reveal the upstream id in the clear")
	}
	got, ok := unwrapSessionID(key, wrapped, agentID)
	if !ok || got != "upstream-123" {
		t.Fatalf("unwrap = %q, %v; want upstream-123, true", got, ok)
	}
}

func TestSessionCrossCallerMismatch(t *testing.T) {
	key := []byte("k1")
	a := callerIdentity(&caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}})
	b := callerIdentity(&caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "other", Kind: KindAgent}})
	bearer := callerIdentity(&caller{Namespace: "team-a"})

	wrapped := wrapSessionID(key, "sess", a)
	for _, other := range []string{b, bearer} {
		if _, ok := unwrapSessionID(key, wrapped, other); ok {
			t.Fatalf("identity %q resumed a session owned by %q", other, a)
		}
	}
	// Bearer identities must not collide with workload identities by
	// construction of the identity strings.
	if a == bearer || callerIdentity(&caller{Namespace: "ns"}) == callerIdentity(&caller{Namespace: "ns", Workload: &Identity{Namespace: "ns", Name: "x", Kind: KindAgent}}) {
		t.Fatal("identity strings collide across tiers")
	}
}

func TestSessionMalformedAndKeyMismatch(t *testing.T) {
	key := []byte("k1")
	id := callerIdentity(&caller{Namespace: "team-a"})
	wrapped := wrapSessionID(key, "sess", id)

	for _, bad := range []string{"", "no-dot", "a.b.c", "!!!.###", wrapped + "x"} {
		if _, ok := unwrapSessionID(key, bad, id); ok {
			t.Fatalf("malformed id %q verified", bad)
		}
	}
	if _, ok := unwrapSessionID([]byte("other-key"), wrapped, id); ok {
		t.Fatal("a different key verified the MAC")
	}
}
