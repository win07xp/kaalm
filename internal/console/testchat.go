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
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	// clientOnce builds the one pooled client on first use, after the
	// caller has set the fields above.
	clientOnce sync.Once
	client     *http.Client
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
// (docs/src/gateways/api/internal-endpoints.md#get-v1spend).
func (c *GatewayChatClient) WorkloadSpend(ctx context.Context, namespace string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/spend?namespace="+url.QueryEscape(namespace), nil)
}

// httpClient returns the one pooled client every call shares, so calls
// reuse a connection instead of each paying a TLS handshake and leaving an
// idle connection behind. The TLS config reads the console identity and the
// CA bundle from the loader on each handshake, so a rotated certificate or
// bundle applies to the next connection with no new client.
func (c *GatewayChatClient) httpClient() *http.Client {
	c.clientOnce.Do(func() {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// The gateway is dialed directly over HTTP/1.1, as before the client
		// was pooled.
		transport.Proxy = nil
		transport.ForceAttemptHTTP2 = false
		transport.TLSClientConfig = c.tlsConfig()
		c.client = &http.Client{Timeout: timeout, Transport: transport}
	})
	return c.client
}

// tlsConfig builds the gateway TLS config. With a loader, the client
// certificate comes from GetClientCertificate and the gateway chain is
// verified in VerifyConnection against the current CA pool and the pinned
// ServerName, or the BaseURL host when none is set; the standard
// verification is skipped only because VerifyConnection replaces it.
func (c *GatewayChatClient) tlsConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.Insecure {
		cfg.InsecureSkipVerify = true // dev/test only
	}
	loader := c.Loader
	if loader == nil {
		return cfg
	}
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return loader.Certificate()
	}
	if c.Insecure {
		return cfg
	}
	cfg.InsecureSkipVerify = true // replaced by VerifyConnection below
	// The name to verify is fixed here, not read from the connection state:
	// the handshake leaves ConnectionState.ServerName empty for an IP host,
	// and an empty name would skip the host check.
	verifyName := c.ServerName
	if verifyName == "" {
		if u, err := url.Parse(c.BaseURL); err == nil {
			verifyName = u.Hostname()
		}
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("gateway presented no certificate")
		}
		pool, err := loader.CAPool()
		if err != nil {
			return err
		}
		opts := x509.VerifyOptions{
			Roots:         pool,
			DNSName:       verifyName,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, err = cs.PeerCertificates[0].Verify(opts)
		return err
	}
	return cfg
}

// do runs one mTLS request against the gateway and relays status and body.
func (c *GatewayChatClient) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	if c.Loader != nil {
		// A missing identity fails here, before any connection, as a plain
		// file error rather than a handshake failure.
		if _, err := c.Loader.Certificate(); err != nil {
			return 0, nil, err
		}
		if _, err := c.Loader.CAPool(); err != nil {
			return 0, nil, err
		}
	}
	client := c.httpClient()
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
