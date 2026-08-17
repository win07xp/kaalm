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

package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The chapter's stated authorization values (docs/src/console/overview.md,
// Authentication): SubjectAccessReview results and token re-validation cache
// for 5 minutes; a login session lives at most 24 hours.
const (
	sarCacheTTL     = 5 * time.Minute
	reviewCacheTTL  = 5 * time.Minute
	sessionMaxAge   = 24 * time.Hour
	sessionCookie   = "kaalm_console_session"
	sessionIDLength = 32
)

// Identity is the TokenReview-authenticated caller.
type Identity struct {
	Username string
	Groups   []string
}

// TokenReviewer validates a bearer token against the cluster.
type TokenReviewer interface {
	Review(ctx context.Context, token string) (Identity, error)
}

// KubeTokenReviewer is the production TokenReviewer. Unlike the gateway's
// workload reviewer, it sets no audience: the console accepts ordinary
// ServiceAccount and OIDC user tokens.
type KubeTokenReviewer struct {
	Client kubernetes.Interface
}

// Review runs a TokenReview and returns the authenticated identity.
func (r *KubeTokenReviewer) Review(ctx context.Context, token string) (Identity, error) {
	review, err := r.Client.AuthenticationV1().TokenReviews().Create(ctx,
		&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("token review: %w", err)
	}
	if !review.Status.Authenticated {
		return Identity{}, fmt.Errorf("token not authenticated: %s", review.Status.Error)
	}
	return Identity{Username: review.Status.User.Username, Groups: review.Status.User.Groups}, nil
}

// Authorizer answers one authorization question about one identity.
type Authorizer interface {
	Allowed(ctx context.Context, id Identity, verb, group, resource, namespace string) (bool, error)
}

// KubeAuthorizer is the production Authorizer over SubjectAccessReview.
type KubeAuthorizer struct {
	Client kubernetes.Interface
}

// Allowed runs one SubjectAccessReview.
func (a *KubeAuthorizer) Allowed(ctx context.Context, id Identity, verb, group, resource, namespace string) (bool, error) {
	sar, err := a.Client.AuthorizationV1().SubjectAccessReviews().Create(ctx,
		&authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User:   id.Username,
			Groups: id.Groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace, Verb: verb, Group: group, Resource: resource,
			},
		}}, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("subject access review: %w", err)
	}
	return sar.Status.Allowed, nil
}

type gateKey struct {
	user, namespace, verb string
}

type gateEntry struct {
	allowed bool
	expires time.Time
}

// Gate is the console's authorization gate: viewing a namespace requires
// list on agents.kaalm.io in it; test-chat requires create on
// agentchannels.kaalm.io (a channel is the standing form of what test-chat
// does once). Results are cached per (identity, namespace, verb) for
// sarCacheTTL.
type Gate struct {
	Authz Authorizer

	now   func() time.Time
	mu    sync.Mutex
	cache map[gateKey]gateEntry
}

// NewGate builds a Gate over an Authorizer.
func NewGate(authz Authorizer) *Gate {
	return &Gate{Authz: authz, now: time.Now, cache: map[gateKey]gateEntry{}}
}

// CanView reports whether the identity may view the namespace's panels.
func (g *Gate) CanView(ctx context.Context, id Identity, namespace string) (bool, error) {
	return g.allowed(ctx, id, namespace, "list", "agents")
}

// CanChat reports whether the identity may test-chat agents in the namespace.
func (g *Gate) CanChat(ctx context.Context, id Identity, namespace string) (bool, error) {
	return g.allowed(ctx, id, namespace, "create", "agentchannels")
}

func (g *Gate) allowed(ctx context.Context, id Identity, namespace, verb, resource string) (bool, error) {
	key := gateKey{user: id.Username, namespace: namespace, verb: verb + ":" + resource}
	g.mu.Lock()
	if e, ok := g.cache[key]; ok && g.now().Before(e.expires) {
		g.mu.Unlock()
		return e.allowed, nil
	}
	g.mu.Unlock()

	allowed, err := g.Authz.Allowed(ctx, id, verb, "kaalm.io", resource, namespace)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	g.cache[key] = gateEntry{allowed: allowed, expires: g.now().Add(sarCacheTTL)}
	g.mu.Unlock()
	return allowed, nil
}

// CachingReviewer wraps a TokenReviewer with a hash-keyed result cache so
// per-request Authorization: Bearer callers do not cost one TokenReview per
// request. Only the SHA-256 of the token is kept.
type CachingReviewer struct {
	Reviewer TokenReviewer

	now   func() time.Time
	mu    sync.Mutex
	cache map[[sha256.Size]byte]reviewEntry
}

type reviewEntry struct {
	id      Identity
	expires time.Time
}

// NewCachingReviewer builds the cache over a TokenReviewer.
func NewCachingReviewer(r TokenReviewer) *CachingReviewer {
	return &CachingReviewer{Reviewer: r, now: time.Now, cache: map[[sha256.Size]byte]reviewEntry{}}
}

// Review validates via the cache, falling through to the wrapped reviewer.
// Failures are not cached: a token that fails review is retried next time.
func (c *CachingReviewer) Review(ctx context.Context, token string) (Identity, error) {
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.id, nil
	}
	c.mu.Unlock()

	id, err := c.Reviewer.Review(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	c.mu.Lock()
	c.cache[key] = reviewEntry{id: id, expires: c.now().Add(reviewCacheTTL)}
	c.mu.Unlock()
	return id, nil
}

type session struct {
	token    string
	id       Identity
	created  time.Time
	reviewed time.Time
}

// SessionStore holds login sessions in memory: a console restart means
// logging in again, deliberately. The pasted token is kept only here (never
// logged, never written to disk) and is re-reviewed every reviewCacheTTL so
// a session dies with its token; sessionMaxAge caps the session absolutely.
type SessionStore struct {
	Reviewer TokenReviewer

	now func() time.Time
	mu  sync.Mutex
	m   map[string]*session
}

// NewSessionStore builds an empty store over a TokenReviewer.
func NewSessionStore(r TokenReviewer) *SessionStore {
	return &SessionStore{Reviewer: r, now: time.Now, m: map[string]*session{}}
}

// Create reviews the token and mints a session, returning the cookie value.
func (s *SessionStore) Create(ctx context.Context, token string) (string, Identity, error) {
	id, err := s.Reviewer.Review(ctx, token)
	if err != nil {
		return "", Identity{}, err
	}
	raw := make([]byte, sessionIDLength)
	if _, err := rand.Read(raw); err != nil {
		return "", Identity{}, err
	}
	value := hex.EncodeToString(raw)
	now := s.now()
	s.mu.Lock()
	s.m[value] = &session{token: token, id: id, created: now, reviewed: now}
	s.mu.Unlock()
	return value, id, nil
}

// Resolve returns the session's identity, re-reviewing the stored token when
// its last review is stale. An expired, dead-token, or unknown session
// resolves to false and is forgotten.
func (s *SessionStore) Resolve(ctx context.Context, value string) (Identity, bool) {
	s.mu.Lock()
	sess, ok := s.m[value]
	s.mu.Unlock()
	if !ok {
		return Identity{}, false
	}
	now := s.now()
	if now.Sub(sess.created) > sessionMaxAge {
		s.Delete(value)
		return Identity{}, false
	}
	if now.Sub(sess.reviewed) > reviewCacheTTL {
		id, err := s.Reviewer.Review(ctx, sess.token)
		if err != nil {
			s.Delete(value)
			return Identity{}, false
		}
		s.mu.Lock()
		sess.id, sess.reviewed = id, now
		s.mu.Unlock()
	}
	return sess.id, true
}

// Delete forgets a session (logout, expiry, dead token).
func (s *SessionStore) Delete(value string) {
	s.mu.Lock()
	delete(s.m, value)
	s.mu.Unlock()
}
