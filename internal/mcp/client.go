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
			ClientInfo:      clientInfo{Name: "kaalm", Version: "v1beta1"},
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

// ListTools calls tools/list: within an initialized session against a
// legacy server, or as a self-describing stateless request (per-request
// _meta) when the session carries a modern revision.
func (c *Client) ListTools(ctx context.Context, session Session) ([]Tool, error) {
	params := map[string]any{}
	if IsModern(session.ProtocolVersion) {
		params["_meta"] = meta()
	}
	id := c.nextID.Add(1)
	resp, _, err := c.post(ctx, session, request{
		JSONRPC: jsonrpcVersion, ID: &id, Method: "tools/list", Params: params,
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
func (c *Client) post(ctx context.Context, session Session, msg request) (Response, http.Header, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return Response{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, nil, err
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
	if IsModern(session.ProtocolVersion) {
		// The 2026-07-28 revision requires the method mirrored into a
		// header on every streamable-HTTP POST.
		req.Header.Set("Mcp-Method", msg.Method)
	}

	cl := c.HTTPClient
	if cl == nil {
		cl = http.DefaultClient
	}
	httpResp, err := cl.Do(req)
	if err != nil {
		return Response{}, nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Modern servers carry a JSON-RPC error in 4xx bodies (400 for an
		// unsupported version or a header mismatch, 404 for an unknown
		// method), and that error is the era proof Discover classifies on.
		// Auth statuses stay HTTPError: the credential verdict outranks
		// whatever body came with it.
		if httpResp.StatusCode != http.StatusUnauthorized && httpResp.StatusCode != http.StatusForbidden {
			raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
			var errResp Response
			if json.Unmarshal(raw, &errResp) == nil && errResp.Error != nil {
				return Response{}, nil, errResp.Error
			}
		}
		return Response{}, nil, &HTTPError{StatusCode: httpResp.StatusCode}
	}
	if msg.ID == nil {
		// Notification: any 2xx acceptance is the whole contract.
		return Response{}, httpResp.Header, nil
	}

	rawID, err := json.Marshal(*msg.ID)
	if err != nil {
		return Response{}, nil, err
	}
	resp, err := ParseResponse(httpResp.Header.Get("Content-Type"),
		io.LimitReader(httpResp.Body, maxResponseBytes), rawID)
	if err != nil {
		return Response{}, nil, err
	}
	if resp.Error != nil {
		return Response{}, nil, resp.Error
	}
	return resp, httpResp.Header, nil
}

// ParseResponse decodes the JSON-RPC response matching rawID from an MCP
// streamable-HTTP response body: a plain JSON object, or an SSE stream whose
// events are scanned for the matching response (other events are skipped).
// Shared by the probe client and the broker's tools/list filter.
func ParseResponse(contentType string, r io.Reader, rawID []byte) (Response, error) {
	if strings.HasPrefix(contentType, "text/event-stream") {
		return readSSEResponse(r, rawID)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// readSSEResponse scans an SSE stream for the JSON-RPC response whose id
// matches the request. Other events (server notifications, unrelated ids)
// are skipped; the stream ending without a match is an error.
func readSSEResponse(r io.Reader, rawID []byte) (Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	var data strings.Builder
	flush := func() (Response, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return Response{}, false
		}
		var resp Response
		if err := json.Unmarshal([]byte(data.String()), &resp); err != nil {
			return Response{}, false
		}
		if !IDEqual(resp.ID, rawID) {
			return Response{}, false
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
		return Response{}, err
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	return Response{}, fmt.Errorf("stream ended without a response for request id %s", string(rawID))
}
