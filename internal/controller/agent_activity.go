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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AgentActivity is one replica's view of one agent's two signal sources.
type AgentActivity struct {
	GatewayTraffic *time.Time `json:"gatewayTraffic"`
	Heartbeat      *time.Time `json:"heartbeat"`
}

// ReplicaActivity is one gateway replica's /v1/activity response.
type ReplicaActivity struct {
	StartedAt time.Time                `json:"replicaStartedAt"`
	Agents    map[string]AgentActivity `json:"agents"`
}

// ActivityClient fetches per-namespace activity from the gateway fleet.
// reachable holds one entry per replica that answered; total is how many
// replicas were enumerated. total == 0 or len(reachable) == 0 both mean the
// controller has no activity data and must not make idle transitions.
type ActivityClient interface {
	NamespaceActivity(ctx context.Context, namespace string) (reachable []ReplicaActivity, total int, err error)
}

// activityCacheWindow is the fixed per-namespace cache window: a controller
// constant, not a Helm tunable, well below any practical idleTimeout.
const activityCacheWindow = 15 * time.Second

// GatewayActivityClient is the production ActivityClient: it enumerates
// gateway Pod IPs via the cache, dials each in parallel with the controller's
// client cert (ServerName pinned to the gateway Service DNS so SAN
// verification passes against Pod-IP targets), and caches the merged result
// per namespace for 15 seconds. See
// docs/src/gateways/user/activation-and-activity.md.
type GatewayActivityClient struct {
	Reader            client.Reader
	OperatorNamespace string
	// CertFile/KeyFile/CAFile are the controller's client identity
	// (kaalm-controller-tls) and trust bundle.
	CertFile, KeyFile, CAFile string
	// Port is the gateway LLM listener port (default 8443).
	Port int

	mu     sync.Mutex
	client *http.Client
	cache  map[string]activityCacheEntry
}

type activityCacheEntry struct {
	fetched   time.Time
	reachable []ReplicaActivity
	total     int
}

func (g *GatewayActivityClient) httpClient() (*http.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		return g.client, nil
	}
	cert, err := tls.LoadX509KeyPair(g.CertFile, g.KeyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(g.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from %s", g.CAFile)
	}
	g.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			// Pod-IP dials: SAN verification runs against the Service DNS.
			ServerName: fmt.Sprintf("%s.%s.svc.cluster.local", gatewayServiceName, g.OperatorNamespace),
		}},
	}
	return g.client, nil
}

// NamespaceActivity fans out to every gateway Pod IP, skipping unreachable
// replicas, with a 15-second per-namespace cache in front.
func (g *GatewayActivityClient) NamespaceActivity(ctx context.Context, namespace string) ([]ReplicaActivity, int, error) {
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]activityCacheEntry{}
	}
	if entry, ok := g.cache[namespace]; ok && time.Since(entry.fetched) < activityCacheWindow {
		g.mu.Unlock()
		return entry.reachable, entry.total, nil
	}
	g.mu.Unlock()

	var pods corev1.PodList
	if err := g.Reader.List(ctx, &pods, client.InNamespace(g.OperatorNamespace),
		client.MatchingLabels(gatewayPodLabels)); err != nil {
		return nil, 0, err
	}
	httpClient, err := g.httpClient()
	if err != nil {
		return nil, 0, err
	}
	port := g.Port
	if port == 0 {
		port = gatewayPort
	}

	type result struct {
		replica ReplicaActivity
		ok      bool
	}
	var targets []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.PodIP != "" && p.DeletionTimestamp.IsZero() {
			targets = append(targets, p.Status.PodIP)
		}
	}
	results := make(chan result, len(targets))
	for _, ip := range targets {
		go func(ip string) {
			url := fmt.Sprintf("https://%s:%d/v1/activity?namespace=%s", ip, port, namespace)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				results <- result{}
				return
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				results <- result{}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			var replica ReplicaActivity
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&replica) != nil {
				results <- result{}
				return
			}
			results <- result{replica: replica, ok: true}
		}(ip)
	}
	var reachable []ReplicaActivity
	for range targets {
		if r := <-results; r.ok {
			reachable = append(reachable, r.replica)
		}
	}

	g.mu.Lock()
	g.cache[namespace] = activityCacheEntry{fetched: time.Now(), reachable: reachable, total: len(targets)}
	g.mu.Unlock()
	return reachable, len(targets), nil
}

// mergedActivity merges one agent's timestamps across replicas (most recent
// per source), then applies the activitySource filter. The order is
// load-bearing: merge first, filter second; the controller owns the policy.
func mergedActivity(reachable []ReplicaActivity, agentName, source string) *time.Time {
	var traffic, heartbeat *time.Time
	newer := func(current, candidate *time.Time) *time.Time {
		if candidate == nil {
			return current
		}
		if current == nil || candidate.After(*current) {
			return candidate
		}
		return current
	}
	for _, replica := range reachable {
		if a, ok := replica.Agents[agentName]; ok {
			traffic = newer(traffic, a.GatewayTraffic)
			heartbeat = newer(heartbeat, a.Heartbeat)
		}
	}
	switch source {
	case "agentHeartbeat":
		return heartbeat
	case "both":
		return newer(traffic, heartbeat)
	default: // gatewayTraffic
		return traffic
	}
}
