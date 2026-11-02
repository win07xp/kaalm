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

package gcpauth

import (
	"testing"
	"time"
)

func TestTokenCache(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newCache := func(clock *time.Time) *TokenCache {
		return &TokenCache{Now: func() time.Time { return *clock }}
	}
	hourToken := func(tok string, now time.Time) Token {
		return Token{AccessToken: tok, Expiry: now.Add(time.Hour)}
	}
	expectHit := func(t *testing.T, c *TokenCache, name, cred, want string) {
		t.Helper()
		got, ok := c.Cached(name, cred)
		if !ok || got != want {
			t.Fatalf("Cached(%q) = %q, %v; want %q, true", name, got, ok, want)
		}
	}
	expectMiss := func(t *testing.T, c *TokenCache, name, cred string) {
		t.Helper()
		if got, ok := c.Cached(name, cred); ok {
			t.Fatalf("Cached(%q) = %q, true; want a miss", name, got)
		}
	}

	t.Run("zero value misses", func(t *testing.T) {
		var c TokenCache
		expectMiss(t, &c, "p", "key")
	})

	t.Run("hit until the refresh margin", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Store("p", "key", hourToken("tok", clock))
		expectHit(t, c, "p", "key", "tok")
		clock = start.Add(54 * time.Minute)
		expectHit(t, c, "p", "key", "tok")
		clock = start.Add(56 * time.Minute)
		expectMiss(t, c, "p", "key")
	})

	t.Run("expiry exactly at the margin misses", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Store("p", "key", Token{AccessToken: "tok", Expiry: clock.Add(TokenRefreshMargin)})
		expectMiss(t, c, "p", "key")
	})

	t.Run("no expiry lasts the default lifetime", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Store("p", "key", Token{AccessToken: "tok"})
		clock = start.Add(54 * time.Minute)
		expectHit(t, c, "p", "key", "tok")
		clock = start.Add(56 * time.Minute)
		expectMiss(t, c, "p", "key")
	})

	t.Run("another credential misses and a store overwrites", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Store("p", "old-key", hourToken("old", clock))
		expectMiss(t, c, "p", "new-key")
		c.Store("p", "new-key", hourToken("new", clock))
		expectHit(t, c, "p", "new-key", "new")
		expectMiss(t, c, "p", "old-key")
	})

	t.Run("names do not share entries", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Store("a", "key", hourToken("tok-a", clock))
		expectMiss(t, c, "b", "key")
		c.Store("b", "key", hourToken("tok-b", clock))
		expectHit(t, c, "a", "key", "tok-a")
		expectHit(t, c, "b", "key", "tok-b")
	})

	t.Run("forget drops only that name", func(t *testing.T) {
		clock := start
		c := newCache(&clock)
		c.Forget("unknown")
		c.Store("a", "key", hourToken("tok-a", clock))
		c.Store("b", "key", hourToken("tok-b", clock))
		c.Forget("a")
		c.Forget("unknown")
		expectMiss(t, c, "a", "key")
		expectHit(t, c, "b", "key", "tok-b")
	})

	t.Run("nil clock uses the wall clock", func(t *testing.T) {
		var c TokenCache
		c.Store("p", "key", Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)})
		expectHit(t, &c, "p", "key", "tok")
	})
}
