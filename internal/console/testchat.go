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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/win07xp/kaalm/internal/tlsutil"
)

// GatewayClient is the console's mTLS client surface on the gateway: one
// governed write (test-chat) and one read (the per-workload spend view).
// Status codes and bodies relay verbatim: the gateway's responses are
// exactly the documented wire contract, so the console never re-maps them.
type GatewayClient interface {
	Chat(ctx context.Context, namespace, agent, userID, content string) (int, []byte, error)
	WorkloadSpend(ctx context.Context, namespace string) (int, []byte, error)
}

// GatewayChatClient is the production GatewayClient: the gateway's cluster
// listener over mTLS, presenting kaalm-console-tls.
type GatewayChatClient struct {
	// BaseURL is the gateway cluster listener, e.g.
	// https://kaalm-gateway.kaalm-system.svc.cluster.local:8443.
	BaseURL string
	// Loader carries the console's TLS identity; nil (with Insecure) for
	// dev/test.
	Loader *tlsutil.CertLoader
	// ServerName pins the gateway Service DNS for verification.
	ServerName string
	// Insecure skips gateway cert verification (dev/test only).
	Insecure bool
	// Timeout bounds one chat round trip, agent wake included. The gateway's
	// own syncDeliveryDeadline settles first in production; this is the
	// client-side backstop.
	Timeout time.Duration
}

// NewGatewayChatClient builds the production client from the console's TLS
// identity, pinned to the gateway Service DNS.
func NewGatewayChatClient(operatorNamespace, certFile, keyFile, caFile string) *GatewayChatClient {
	host := fmt.Sprintf("kaalm-gateway.%s.svc.cluster.local", operatorNamespace)
	return &GatewayChatClient{
		BaseURL:    fmt.Sprintf("https://%s:8443", host),
		Loader:     &tlsutil.CertLoader{CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
		ServerName: host,
		Timeout:    2 * time.Minute,
	}
}

// Chat posts one message. The content never enters an error or a log.
func (c *GatewayChatClient) Chat(ctx context.Context, namespace, agent, userID, content string) (int, []byte, error) {
	payload, err := json.Marshal(map[string]string{
		"namespace": namespace, "agent": agent, "userId": userID, "content": content,
	})
	if err != nil {
		return 0, nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/test-chat", payload)
}

// WorkloadSpend reads the per-workload spend view for one namespace
// (docs/src/gateways/api/internal-endpoints.md, GET /v1/spend).
func (c *GatewayChatClient) WorkloadSpend(ctx context.Context, namespace string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/spend?namespace="+url.QueryEscape(namespace), nil)
}

// do runs one mTLS request against the gateway and relays status and body.
func (c *GatewayChatClient) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.Insecure {
		tlsCfg.InsecureSkipVerify = true // dev/test only
	}
	if c.Loader != nil {
		cert, err := c.Loader.Certificate()
		if err != nil {
			return 0, nil, err
		}
		pool, err := c.Loader.CAPool()
		if err != nil {
			return 0, nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{*cert}
		tlsCfg.RootCAs = pool
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	target := strings.TrimSuffix(c.BaseURL, "/") + path
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}
