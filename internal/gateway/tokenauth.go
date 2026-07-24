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
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// tokenAudience is the audience the gateway requires on projected SA
	// tokens; generic kubernetes.default.svc tokens are rejected.
	tokenAudience = "kaalm-gateway"

	tokenCacheMaxTTL       = 5 * time.Minute
	tokenCacheSafetyMargin = 60 * time.Second
)

// TokenReviewer validates a bearer token and returns the authenticated
// ServiceAccount username (system:serviceaccount:<ns>:<name>). Injected so
// tests need no apiserver.
type TokenReviewer interface {
	Review(ctx context.Context, token string) (username string, authenticated bool, err error)
}

// KubeTokenReviewer is the production TokenReviewer backed by the
// authentication.k8s.io/v1 TokenReview API.
type KubeTokenReviewer struct {
	Client kubernetes.Interface
}

// Review posts a TokenReview with the kaalm-gateway audience.
func (k *KubeTokenReviewer) Review(ctx context.Context, token string) (string, bool, error) {
	tr := &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{tokenAudience}},
	}
	res, err := k.Client.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", false, err
	}
	return res.Status.User.Username, res.Status.Authenticated, nil
}

// TokenAuthenticator caches TokenReview results keyed by the token's SHA-256.
// The TTL derives from the token's own exp claim (parsed locally WITHOUT
// signature verification, which is safe because the apiserver already
// authenticated the token and the claim only bounds cache lifetime), minus a
// 60s margin, capped at 5 minutes.
type TokenAuthenticator struct {
	Reviewer TokenReviewer

	mu    sync.Mutex
	cache map[[32]byte]tokenCacheEntry
	// now is injectable for tests.
	now func() time.Time
}

type tokenCacheEntry struct {
	namespace string
	expires   time.Time
}

// NewTokenAuthenticator builds an authenticator over the given reviewer.
func NewTokenAuthenticator(reviewer TokenReviewer) *TokenAuthenticator {
	return &TokenAuthenticator{Reviewer: reviewer, cache: map[[32]byte]tokenCacheEntry{}, now: time.Now}
}

// Authenticate resolves a bearer token to its ServiceAccount namespace.
// ok=false means the token was rejected (401); err!=nil means the apiserver
// was unreachable (503 internal_unavailable).
func (a *TokenAuthenticator) Authenticate(ctx context.Context, token string) (namespace string, ok bool, err error) {
	key := sha256.Sum256([]byte(token))
	a.mu.Lock()
	if entry, hit := a.cache[key]; hit && a.now().Before(entry.expires) {
		a.mu.Unlock()
		return entry.namespace, true, nil
	}
	a.mu.Unlock()

	username, authenticated, err := a.Reviewer.Review(ctx, token)
	if err != nil {
		return "", false, err
	}
	if !authenticated {
		return "", false, nil
	}
	ns, ok := namespaceFromUsername(username)
	if !ok {
		return "", false, nil
	}

	a.mu.Lock()
	now := a.now()
	a.cache[key] = tokenCacheEntry{namespace: ns, expires: now.Add(cacheTTL(token, now))}
	a.mu.Unlock()
	return ns, true, nil
}

// namespaceFromUsername parses system:serviceaccount:<namespace>:<name>.
func namespaceFromUsername(username string) (string, bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}

// cacheTTL derives the cache lifetime from the JWT exp claim. A token that
// does not parse as a JWT gets the fixed maximum TTL.
func cacheTTL(token string, now time.Time) time.Duration {
	exp, ok := jwtExpiry(token)
	if !ok {
		return tokenCacheMaxTTL
	}
	ttl := exp.Sub(now) - tokenCacheSafetyMargin
	if ttl > tokenCacheMaxTTL {
		return tokenCacheMaxTTL
	}
	if ttl < 0 {
		return 0
	}
	return ttl
}

// jwtExpiry extracts the exp claim from a JWT payload without verifying the
// signature.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// bearerToken extracts the token from an Authorization: Bearer header value.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || len(header) == len(prefix) {
		return "", false
	}
	return header[len(prefix):], true
}
