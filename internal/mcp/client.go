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

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// maxResponseBytes bounds how much of an upstream response the client reads.
// The probe's answers are small; anything larger is a misbehaving server.
const maxResponseBytes = 1 << 20

// Client speaks MCP streamable HTTP to one server. The zero HTTPClient falls
// back to http.DefaultClient; bound calls with a context deadline or a
// configured client instead of relying on server behavior.
type Client struct {
	// Endpoint is the server URL, POSTed to directly.
	Endpoint string
	// Credential, when non-empty, is sent as an Authorization bearer token.
	Credential string
	// HTTPClient issues the requests. Nil means http.DefaultClient.
	HTTPClient *http.Client

	nextID atomic.Int64
}

// Initialize runs the MCP handshake: the initialize call, then the
// notifications/initialized notification. It returns the session the server
// established (possibly with an empty ID) for use on subsequent calls.
func (c *Client) Initialize(ctx context.Context) (Session, error) {
	id := c.nextID.Add(1)
	resp, header, err := c.post(ctx, Session{}, request{
		JSONRPC: jsonrpcVersion, ID: &id, Method: "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{},
			ClientInfo:      clientInfo{Name: "kaalm", Version: "v1alpha1"},
		},
	})
	if err != nil {
		return Session{}, fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return Session{}, fmt.Errorf("initialize: malformed result: %w", err)
	}
	session := Session{ID: header.Get("Mcp-Session-Id"), ProtocolVersion: res.ProtocolVersion}

	if _, _, err := c.post(ctx, session, request{
		JSONRPC: jsonrpcVersion, Method: "notifications/initialized",
	}); err != nil {
		return Session{}, fmt.Errorf("notifications/initialized: %w", err)
	}
	return session, nil
}

// ListTools calls tools/list within an initialized session.
func (c *Client) ListTools(ctx context.Context, session Session) ([]Tool, error) {
	id := c.nextID.Add(1)
	resp, _, err := c.post(ctx, session, request{
		JSONRPC: jsonrpcVersion, ID: &id, Method: "tools/list", Params: map[string]any{},
	})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var res listToolsResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("tools/list: malformed result: %w", err)
	}
	return res.Tools, nil
}

// post sends one JSON-RPC message and returns the matching response. For a
// notification (nil ID) it returns a zero response once the server accepts
// the POST. A JSON-RPC error object is returned as an *RPCError, a non-2xx
// status as an *HTTPError.
func (c *Client) post(ctx context.Context, session Session, msg request) (response, http.Header, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return response{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return response{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	}
	if session.ID != "" {
		req.Header.Set("Mcp-Session-Id", session.ID)
	}
	if session.ProtocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", session.ProtocolVersion)
	}

	cl := c.HTTPClient
	if cl == nil {
		cl = http.DefaultClient
	}
	httpResp, err := cl.Do(req)
	if err != nil {
		return response{}, nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return response{}, nil, &HTTPError{StatusCode: httpResp.StatusCode}
	}
	if msg.ID == nil {
		// Notification: any 2xx acceptance is the whole contract.
		return response{}, httpResp.Header, nil
	}

	reader := io.LimitReader(httpResp.Body, maxResponseBytes)
	var resp response
	ct := httpResp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		resp, err = readSSEResponse(reader, *msg.ID)
	default:
		var raw []byte
		if raw, err = io.ReadAll(reader); err == nil {
			err = json.Unmarshal(raw, &resp)
		}
	}
	if err != nil {
		return response{}, nil, err
	}
	if resp.Error != nil {
		return response{}, nil, resp.Error
	}
	return resp, httpResp.Header, nil
}

// readSSEResponse scans an SSE stream for the JSON-RPC response whose id
// matches the request. Other events (server notifications, unrelated ids)
// are skipped; the stream ending without a match is an error.
func readSSEResponse(r io.Reader, wantID int64) (response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	var data strings.Builder
	flush := func() (response, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return response{}, false
		}
		var resp response
		if err := json.Unmarshal([]byte(data.String()), &resp); err != nil {
			return response{}, false
		}
		if resp.ID == nil || *resp.ID != wantID {
			return response{}, false
		}
		return resp, true
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if resp, ok := flush(); ok {
				return resp, nil
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return response{}, err
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	return response{}, fmt.Errorf("stream ended without a response for request %d", wantID)
}
