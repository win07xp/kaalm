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
	"crypto/x509"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func certWithSANs(sans ...string) *x509.Certificate {
	return &x509.Certificate{DNSNames: sans}
}

func TestParseWorkloadSAN(t *testing.T) {
	cases := []struct {
		name    string
		sans    []string
		want    Identity
		wantErr bool
	}{
		{
			name: "agent cert with short forms ignored",
			sans: []string{"sup.team-a.svc.cluster.local", "sup.team-a.svc", "sup.team-a"},
			want: Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent},
		},
		{
			name: "task cert",
			sans: []string{"fix-42.team-b.task.agentry.io"},
			want: Identity{Namespace: "team-b", Name: "fix-42", Kind: KindAgentTask},
		},
		{
			name:    "dotted-name spoof has 6 labels and is rejected",
			sans:    []string{"admin.svc.team-a.svc.cluster.local"},
			wantErr: true,
		},
		{
			name:    "task shape with extra label rejected",
			sans:    []string{"a.b.c.task.agentry.io"},
			wantErr: true,
		},
		{
			name:    "no recognized SAN",
			sans:    []string{"agentry-gateway.agentry-system.svc", "localhost"},
			wantErr: true,
		},
		{
			name:    "two recognized SANs rejected",
			sans:    []string{"a.ns1.svc.cluster.local", "b.ns2.task.agentry.io"},
			wantErr: true,
		},
		{
			name:    "empty SAN list",
			sans:    nil,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseWorkloadSAN(certWithSANs(c.sans...))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestIsControllerCert(t *testing.T) {
	long := certWithSANs("agentry-controller.agentry-system.svc.cluster.local")
	short := certWithSANs("agentry-controller.agentry-system.svc")
	agent := certWithSANs("sup.team-a.svc.cluster.local")
	if !IsControllerCert(long, "agentry-system") || !IsControllerCert(short, "agentry-system") {
		t.Error("controller SANs must be accepted")
	}
	if IsControllerCert(agent, "agentry-system") {
		t.Error("agent cert must not pass the controller check")
	}
	if IsControllerCert(long, "other-ns") {
		t.Error("controller SAN for a different namespace must not pass")
	}
}

type fakeReviewer struct {
	username      string
	authenticated bool
	err           error
	calls         int
}

func (f *fakeReviewer) Review(_ context.Context, _ string) (string, bool, error) {
	f.calls++
	return f.username, f.authenticated, f.err
}

func TestTokenAuthenticator_CachesByTTL(t *testing.T) {
	rev := &fakeReviewer{username: "system:serviceaccount:team-a:runner", authenticated: true}
	auth := NewTokenAuthenticator(rev)
	now := time.Now()
	auth.now = func() time.Time { return now }

	ns, ok, err := auth.Authenticate(context.Background(), "opaque-token")
	if err != nil || !ok || ns != "team-a" {
		t.Fatalf("first auth failed: ns=%q ok=%v err=%v", ns, ok, err)
	}
	// Second call within TTL hits the cache.
	if _, _, _ = auth.Authenticate(context.Background(), "opaque-token"); rev.calls != 1 {
		t.Fatalf("expected cache hit, reviewer called %d times", rev.calls)
	}
	// Advance past the max TTL: reviewer is consulted again.
	now = now.Add(tokenCacheMaxTTL + time.Second)
	if _, _, _ = auth.Authenticate(context.Background(), "opaque-token"); rev.calls != 2 {
		t.Fatalf("expected cache expiry, reviewer called %d times", rev.calls)
	}
}

func TestTokenAuthenticator_RejectsAndErrors(t *testing.T) {
	rejected := NewTokenAuthenticator(&fakeReviewer{authenticated: false})
	if _, ok, err := rejected.Authenticate(context.Background(), "t"); ok || err != nil {
		t.Error("unauthenticated token must be rejected without error")
	}
	badUser := NewTokenAuthenticator(&fakeReviewer{username: "system:node:worker-1", authenticated: true})
	if _, ok, _ := badUser.Authenticate(context.Background(), "t"); ok {
		t.Error("non-serviceaccount username must be rejected")
	}
	failing := NewTokenAuthenticator(&fakeReviewer{err: context.DeadlineExceeded})
	if _, _, err := failing.Authenticate(context.Background(), "t"); err == nil {
		t.Error("apiserver failure must surface as error")
	}
}

func TestNamespaceFromUsername(t *testing.T) {
	if ns, ok := namespaceFromUsername("system:serviceaccount:team-a:runner"); !ok || ns != "team-a" {
		t.Errorf("parse failed: %q %v", ns, ok)
	}
	for _, bad := range []string{"system:node:x", "system:serviceaccount::runner", "team-a:runner", ""} {
		if _, ok := namespaceFromUsername(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	// Whole seconds: the exp claim carries Unix-second resolution.
	now := time.Unix(time.Now().Unix(), 0)
	// Not a JWT: fixed max TTL.
	if got := cacheTTL("opaque", now); got != tokenCacheMaxTTL {
		t.Errorf("opaque token TTL = %v, want %v", got, tokenCacheMaxTTL)
	}
	// JWT with a 2-minute expiry: TTL = 2m - 60s margin = 1m.
	token := jwtWithExp(now.Add(2 * time.Minute))
	if got := cacheTTL(token, now); got != time.Minute {
		t.Errorf("short-lived token TTL = %v, want 1m", got)
	}
	// JWT with a 1-hour expiry: capped at 5m.
	token = jwtWithExp(now.Add(time.Hour))
	if got := cacheTTL(token, now); got != tokenCacheMaxTTL {
		t.Errorf("long-lived token TTL = %v, want cap %v", got, tokenCacheMaxTTL)
	}
	// Already-expired JWT: zero.
	token = jwtWithExp(now.Add(-time.Minute))
	if got := cacheTTL(token, now); got != 0 {
		t.Errorf("expired token TTL = %v, want 0", got)
	}
}

func jwtWithExp(exp time.Time) string {
	// header.payload.signature with only the payload meaningful.
	payload := []byte(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
