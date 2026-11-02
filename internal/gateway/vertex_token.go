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
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/win07xp/kaalm/internal/gcpauth"
)

// vertexTokenCache holds one minted access token per google-vertex
// provider, so the gateway mints about once an hour per provider instead of
// once per request. The caching policy (key hash, refresh margin, default
// lifetime) is gcpauth.TokenCache; this type adds a single flight, so
// concurrent misses on one provider share one mint. The zero value is ready
// to use.
type vertexTokenCache struct {
	cache  gcpauth.TokenCache
	flight singleflight.Group
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
	if tok, ok := c.cache.Cached(providerName, credential); ok {
		return tok, nil
	}
	ch := c.flight.DoChan(providerName, func() (any, error) {
		// A caller that missed the cache just before the last mint stored
		// its token finds it here.
		if tok, ok := c.cache.Cached(providerName, credential); ok {
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
		c.cache.Store(providerName, credential, minted)
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
