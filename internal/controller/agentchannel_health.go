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
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GatewayChannelHealthClient is the production ChannelHealthClient: the same
// fan-out as the activity client (every gateway Pod IP in parallel, the
// controller's client cert, SAN verification pinned to the Service DNS) at
// GET /v1/channels/health, with the same 15-second per-namespace cache. See
// docs/src/gateways/user/platform-adapters.md (Channel Health Tracking) and
// docs/src/controller/reconcilers.md (Channel health poll).
type GatewayChannelHealthClient struct {
	Reader            client.Reader
	OperatorNamespace string
	// CertFile/KeyFile/CAFile are the controller's client identity
	// (kaalm-controller-tls) and trust bundle.
	CertFile, KeyFile, CAFile string
	// Port is the gateway cluster listener port (default 8443).
	Port int

	mu     sync.Mutex
	client *http.Client
	cache  map[string]channelHealthCacheEntry
}

type channelHealthCacheEntry struct {
	fetched   time.Time
	reachable []ReplicaChannelHealth
	total     int
}

func (g *GatewayChannelHealthClient) httpClient() (*http.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		return g.client, nil
	}
	c, err := newGatewayMTLSClient(g.CertFile, g.KeyFile, g.CAFile, g.OperatorNamespace)
	if err != nil {
		return nil, err
	}
	g.client = c
	return g.client, nil
}

// NamespaceChannelHealth fans out to every gateway Pod IP, skipping
// unreachable replicas, with a 15-second per-namespace cache in front.
func (g *GatewayChannelHealthClient) NamespaceChannelHealth(
	ctx context.Context, namespace string,
) ([]ReplicaChannelHealth, int, error) {
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]channelHealthCacheEntry{}
	}
	if entry, ok := g.cache[namespace]; ok && time.Since(entry.fetched) < activityCacheWindow {
		g.mu.Unlock()
		return entry.reachable, entry.total, nil
	}
	g.mu.Unlock()

	httpClient, err := g.httpClient()
	if err != nil {
		return nil, 0, err
	}
	reachable, total, err := gatewayFanOut[ReplicaChannelHealth](ctx, g.Reader, g.OperatorNamespace, httpClient, g.Port,
		"/v1/channels/health", namespace)
	if err != nil {
		return nil, 0, err
	}

	g.mu.Lock()
	g.cache[namespace] = channelHealthCacheEntry{fetched: time.Now(), reachable: reachable, total: total}
	g.mu.Unlock()
	return reachable, total, nil
}
