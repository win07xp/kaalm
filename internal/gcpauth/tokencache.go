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
	"crypto/sha256"
	"sync"
	"time"
)

const (
	// TokenRefreshMargin is how long before its expiry a cached access token
	// is replaced, so no request goes out with a token about to lapse.
	TokenRefreshMargin = 5 * time.Minute
	// DefaultTokenLifetime is Google's documented access-token lifetime,
	// assumed when the token endpoint reports no expiry.
	DefaultTokenLifetime = time.Hour
)

// TokenCache holds one access token per name (a provider), so a caller mints
// about once an hour per name instead of once per use. An entry is tied to
// the SHA-256 hash of the credential it was minted from, so a rotated
// credential misses and mints again. The zero value is ready to use, and a
// TokenCache is safe for concurrent use. It does not mint: the caller mints
// on a miss and stores the result.
type TokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken // key: name
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type cachedToken struct {
	keyHash [sha256.Size]byte
	token   string
	expiry  time.Time
}

func (c *TokenCache) clock() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Cached returns the token stored under name when it was minted from this
// credential and does not expire within TokenRefreshMargin.
func (c *TokenCache) Cached(name, credential string) (string, bool) {
	hash := sha256.Sum256([]byte(credential))
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[name]
	if !ok || t.keyHash != hash || !t.expiry.After(c.clock().Add(TokenRefreshMargin)) {
		return "", false
	}
	return t.token, true
}

// Store records tok, minted from credential, under name, replacing any
// earlier entry. A token with no expiry is kept for DefaultTokenLifetime.
func (c *TokenCache) Store(name, credential string, tok Token) {
	hash := sha256.Sum256([]byte(credential))
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry := tok.Expiry
	if expiry.IsZero() {
		expiry = c.clock().Add(DefaultTokenLifetime)
	}
	if c.tokens == nil {
		c.tokens = make(map[string]cachedToken)
	}
	c.tokens[name] = cachedToken{keyHash: hash, token: tok.AccessToken, expiry: expiry}
}

// Forget drops the token stored under name, so the next use mints again.
func (c *TokenCache) Forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, name)
}
