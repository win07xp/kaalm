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
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/win07xp/kaalm/internal/gcpauth"
)

const (
	// vertexTokenRefreshMargin is how long before its expiry a cached access
	// token is replaced, so no request goes out with a token about to lapse.
	vertexTokenRefreshMargin = 5 * time.Minute
	// vertexTokenDefaultLifetime is Google's documented access-token
	// lifetime, assumed when the token endpoint reports no expiry.
	vertexTokenDefaultLifetime = time.Hour
)

// vertexTokenCache holds one minted access token per google-vertex
// provider, so the gateway mints about once an hour per provider instead of
// once per request. An entry is tied to the hash of the key it was minted
// from, so a rotated Secret re-mints on the next request. Concurrent misses
// on one provider share a single mint. The zero value is ready to use.
type vertexTokenCache struct {
	mu     sync.Mutex
	tokens map[string]vertexToken // key: provider name
	flight singleflight.Group
	// now is the clock; nil means time.Now.
	now func() time.Time
}

type vertexToken struct {
	keyHash [sha256.Size]byte
	token   string
	expiry  time.Time
}

func (c *vertexTokenCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// cached returns the provider's token when it was minted from the key with
// this hash and is not inside the refresh margin.
func (c *vertexTokenCache) cached(providerName string, hash [sha256.Size]byte) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[providerName]
	if !ok || t.keyHash != hash || !t.expiry.After(c.clock().Add(vertexTokenRefreshMargin)) {
		return "", false
	}
	return t.token, true
}

// token returns an access token for the provider's service-account key
// (credential), minting one over hc when none is cached. A key that does not
// parse wraps gcpauth.ErrInvalidKey and a token endpoint that refuses it
// wraps gcpauth.ErrRejected. The mint runs detached from ctx, bounded by
// timeout, so a caller that leaves does not fail the mint other callers
// share; that caller's own wait still ends with ctx.
func (c *vertexTokenCache) token(
	ctx context.Context, hc *http.Client, providerName, credential string, timeout time.Duration,
) (string, error) {
	hash := sha256.Sum256([]byte(credential))
	if tok, ok := c.cached(providerName, hash); ok {
		return tok, nil
	}
	ch := c.flight.DoChan(providerName, func() (any, error) {
		// A caller that missed the cache just before the last mint stored
		// its token finds it here.
		if tok, ok := c.cached(providerName, hash); ok {
			return tok, nil
		}
		key, err := gcpauth.ParseServiceAccountKey([]byte(credential))
		if err != nil {
			return "", err
		}
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		minted, err := key.Token(mctx, hc)
		if err != nil {
			return "", err
		}
		expiry := minted.Expiry
		if expiry.IsZero() {
			expiry = c.clock().Add(vertexTokenDefaultLifetime)
		}
		c.mu.Lock()
		if c.tokens == nil {
			c.tokens = map[string]vertexToken{}
		}
		c.tokens[providerName] = vertexToken{keyHash: hash, token: minted.AccessToken, expiry: expiry}
		c.mu.Unlock()
		return minted.AccessToken, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
