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

// Package mcp is the minimal MCP (Model Context Protocol) streamable-HTTP
// client Kaalm needs: JSON-RPC over POST with plain-JSON or SSE responses,
// the initialize handshake, and tools/list. The ToolProvider health probe
// speaks it today; the gateway's tool broker builds on the same wire types.
// Like internal/callbackpolicy, it is a leaf package shared by the
// controller and gateway planes, which must not import each other.
// See docs/src/gateways/tool-plane.md.
package mcp

import (
	"encoding/json"
	"fmt"
)

// protocolVersion is the MCP revision this client speaks: the one that
// introduced the streamable HTTP transport. The server answers initialize
// with the version it selects, echoed back on later requests.
const protocolVersion = "2025-03-26"

// jsonrpcVersion is the fixed JSON-RPC 2.0 version marker.
const jsonrpcVersion = "2.0"

// request is a JSON-RPC 2.0 request. A nil ID marks a notification.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a decoded JSON-RPC 2.0 response. ID is kept raw because
// callers relayed by the broker may use string or numeric ids.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC 2.0 error object, surfaced verbatim to callers.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// HTTPError is a non-2xx transport response. Callers classify credential
// problems by its StatusCode (401 and 403).
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("server returned HTTP %d", e.StatusCode)
}

// Session is the negotiated state Initialize returns. ID is empty when the
// server issued no Mcp-Session-Id (stateless servers are valid).
type Session struct {
	ID              string
	ProtocolVersion string
}

// initializeParams is the client half of the initialize handshake.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeResult is the subset of the server's initialize answer the
// client reads.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// Tool is one entry of a server's tools/list answer.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// listToolsResult is the subset of the tools/list answer the client reads.
// Pagination (nextCursor) is deliberately ignored: the probe needs proof the
// method works, not the full catalog.
type listToolsResult struct {
	Tools []Tool `json:"tools"`
}

// IDEqual reports whether two raw JSON-RPC ids denote the same id, comparing
// decoded values so formatting differences (whitespace, number forms) do not
// break the match. A nil or literal-null id never matches.
func IDEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	// JSON-RPC ids are strings or numbers; anything else (null, or an
	// illegal composite) never matches, and composites must not reach ==.
	switch at := av.(type) {
	case string:
		bt, ok := bv.(string)
		return ok && at == bt
	case float64:
		bt, ok := bv.(float64)
		return ok && at == bt
	}
	return false
}
