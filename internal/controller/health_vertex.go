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
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/win07xp/kaalm/internal/gcpauth"
)

// vertexRegionalHostSuffix is the host suffix of a regional Vertex endpoint,
// {location}-aiplatform.googleapis.com.
const vertexRegionalHostSuffix = "-aiplatform.googleapis.com"

const (
	// vertexTokenRefreshMargin is how long before its expiry a cached access
	// token is replaced, so no probe goes out with a token about to lapse.
	vertexTokenRefreshMargin = 5 * time.Minute
	// vertexTokenDefaultLifetime is Google's documented access-token
	// lifetime, assumed when the token endpoint reports no expiry.
	vertexTokenDefaultLifetime = time.Hour
)

// vertexProbeTokens holds one minted access token per google-vertex
// provider, so the probe mints about once an hour per provider instead of
// once per probe. An entry is tied to the hash of the key it was minted
// from, so a rotated Secret re-mints on the next probe. Entries stay for the
// controller's lifetime. The zero value is ready to use.
type vertexProbeTokens struct {
	mu     sync.Mutex
	tokens map[string]vertexProbeToken // key: provider name
	// now is the clock; nil means time.Now.
	now func() time.Time
}

type vertexProbeToken struct {
	keyHash [sha256.Size]byte
	token   string
	expiry  time.Time
}

func (c *vertexProbeTokens) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// cached returns the provider's token when it was minted from the key with
// this hash and is not inside the refresh margin.
func (c *vertexProbeTokens) cached(providerName string, hash [sha256.Size]byte) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[providerName]
	if !ok || t.keyHash != hash || !t.expiry.After(c.clock().Add(vertexTokenRefreshMargin)) {
		return "", false
	}
	return t.token, true
}

// store records the token minted for the provider from the key with this
// hash.
func (c *vertexProbeTokens) store(providerName string, hash [sha256.Size]byte, token string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = make(map[string]vertexProbeToken)
	}
	c.tokens[providerName] = vertexProbeToken{keyHash: hash, token: token, expiry: expiry}
}

// forget drops the provider's token, so the next probe mints again.
func (c *vertexProbeTokens) forget(providerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, providerName)
}

// vertexProbeRequest builds the google-vertex liveness request: a GET of the
// publisher-models list,
// {endpoint}/v1/projects/{project}/locations/{location}/publishers/google/models,
// carrying an OAuth2 access token for the service-account JSON key in the
// credential. The project is the key's project_id and the location comes
// from the endpoint host (vertexLocation). The token comes from tokens when
// one minted from this key for this provider is still good; otherwise it is
// minted over hc, so the mint shares the probe's timeout, redirect refusal,
// and trust pool, and stored in tokens. A failed mint stores nothing. A
// credential that is not a usable key, or a token endpoint that refuses it,
// returns an AuthFailed result; any other token failure is transient.
func vertexProbeRequest(
	ctx context.Context, hc *http.Client, tokens *vertexProbeTokens, providerName, endpoint, credential string,
) (*http.Request, *ProviderProbeResult) {
	key, err := gcpauth.ParseServiceAccountKey([]byte(credential))
	if err != nil {
		return nil, &ProviderProbeResult{AuthFailed: true, Err: err}
	}
	hash := sha256.Sum256([]byte(credential))
	token, ok := tokens.cached(providerName, hash)
	if !ok {
		minted, err := key.Token(ctx, hc)
		if err != nil {
			if errors.Is(err, gcpauth.ErrRejected) {
				return nil, &ProviderProbeResult{AuthFailed: true, Err: err}
			}
			return nil, &ProviderProbeResult{Err: fmt.Errorf("minting the Vertex access token: %w", err)}
		}
		expiry := minted.Expiry
		if expiry.IsZero() {
			expiry = tokens.clock().Add(vertexTokenDefaultLifetime)
		}
		tokens.store(providerName, hash, minted.AccessToken, expiry)
		token = minted.AccessToken
	}
	target := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models",
		strings.TrimSuffix(endpoint, "/"), url.PathEscape(key.ProjectID), url.PathEscape(vertexLocation(endpoint)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &ProviderProbeResult{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// vertexLocation derives the Vertex location from the endpoint: the
// {location} of a regional {location}-aiplatform.googleapis.com host, and
// "global" for any other host, including the global
// aiplatform.googleapis.com endpoint.
func vertexLocation(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "global"
	}
	if loc, ok := strings.CutSuffix(u.Hostname(), vertexRegionalHostSuffix); ok && loc != "" {
		return loc
	}
	return "global"
}
