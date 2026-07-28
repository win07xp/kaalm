// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gateway is the handler's window to the Kaalm gateway: an HTTP client
// preconfigured with the Pod's mTLS identity and CA trust, kept current by
// the same rotation watch the serving side uses. A handler makes an LLM call
// by POSTing a qualified model request through it; it never touches
// certificate files.
type Gateway struct {
	endpoint string
	hc       *http.Client
}

func newGateway(endpoint string, r *certReloader) *Gateway {
	return &Gateway{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		hc: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &reloadingTransport{reloader: r},
		},
	}
}

// Endpoint returns the gateway base URL from $KAALM_GATEWAY_ENDPOINT, without
// a trailing slash.
func (g *Gateway) Endpoint() string {
	return g.endpoint
}

// Client returns the underlying mTLS-configured client, for requests the
// convenience methods do not cover.
func (g *Gateway) Client() *http.Client {
	return g.hc
}

// Post sends a JSON POST to the gateway path (for example
// "/v1/chat/completions"). A nil body sends an empty request. The caller owns
// the response and must close its Body.
func (g *Gateway) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gateway: encoding request body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url(path), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.hc.Do(req)
}

// Get sends a GET to the gateway path. The caller owns the response and must
// close its Body.
func (g *Gateway) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url(path), nil)
	if err != nil {
		return nil, err
	}
	return g.hc.Do(req)
}

func (g *Gateway) url(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return g.endpoint + path
}

// reloadingTransport rebuilds its inner transport whenever the cert reloader
// has rotated. GetClientCertificate is live on every dial, but RootCAs in a
// tls.Config is a snapshot with no refresh callback: a transport built before
// a CA rotation would distrust a gateway serving from the new CA forever.
// Comparing generations on each request keeps outbound trust current and
// costs one mutex hop on the happy path.
type reloadingTransport struct {
	reloader *certReloader

	mu    sync.Mutex
	gen   uint64
	inner *http.Transport
}

func (t *reloadingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if gen := t.reloader.generation(); t.inner == nil || gen != t.gen {
		if t.inner != nil {
			t.inner.CloseIdleConnections()
		}
		t.inner = &http.Transport{TLSClientConfig: t.reloader.clientTLSConfig()}
		t.gen = gen
	}
	inner := t.inner
	t.mu.Unlock()
	return inner.RoundTrip(req)
}
