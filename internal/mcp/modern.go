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
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ModernRevision is the stateless MCP revision this client speaks:
// per-request _meta instead of the initialize handshake, no sessions,
// mirrored Mcp-Method and Mcp-Name headers.
const ModernRevision = "2026-07-28"

// IsModern reports whether an MCP protocol revision uses per-request
// metadata rather than the initialize handshake. Revisions are ISO dates,
// so the string comparison orders them.
func IsModern(revision string) bool { return revision >= ModernRevision }

// _meta key names the 2026-07-28 revision defines.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// Modern JSON-RPC error codes from the MCP-reserved range. Receiving one
// proves the server speaks a modern revision; anything else on a failed
// era probe means a legacy server (the revision's own fallback rule).
const (
	CodeHeaderMismatch                  = -32020
	CodeMissingRequiredClientCapability = -32021
	CodeUnsupportedProtocolVersion      = -32022
)

// isModernError reports whether err carries a JSON-RPC error code the MCP
// specification reserves for modern revisions.
func isModernError(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	switch rpcErr.Code {
	case CodeHeaderMismatch, CodeMissingRequiredClientCapability, CodeUnsupportedProtocolVersion:
		return true
	}
	return false
}

// meta is the standard _meta object a modern request carries.
func meta() map[string]any {
	return map[string]any{
		metaProtocolVersion:    ModernRevision,
		metaClientInfo:         map[string]string{"name": "kaalm", "version": "v1beta1"},
		metaClientCapabilities: map[string]any{},
	}
}

// discoverResult is the subset of a server/discover answer the client reads.
type discoverResult struct {
	SupportedVersions []string `json:"supportedVersions"`
}

// Discover probes the server's era: a server/discover success (or a modern
// error answering it) identifies a 2026-07-28 server, and the returned
// Session carries the selected modern revision with no session id. Any
// other failure returns ErrLegacyServer; the caller falls back to
// Initialize. Transport failures return as themselves.
func (c *Client) Discover(ctx context.Context) (Session, error) {
	id := c.nextID.Add(1)
	resp, _, err := c.post(ctx, Session{ProtocolVersion: ModernRevision}, request{
		JSONRPC: jsonrpcVersion, ID: &id, Method: "server/discover",
		Params: map[string]any{"_meta": meta()},
	})
	if err != nil {
		var httpErr *HTTPError
		switch {
		case isModernError(err):
			// A modern server that rejects our version tells us so in its
			// own vocabulary; there is no older modern revision to retry.
			return Session{}, fmt.Errorf("server/discover: %w", err)
		case errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403):
			return Session{}, fmt.Errorf("server/discover: %w", err)
		default:
			return Session{}, fmt.Errorf("%w: %w", ErrLegacyServer, err)
		}
	}
	var res discoverResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return Session{}, fmt.Errorf("%w: malformed server/discover result: %w", ErrLegacyServer, err)
	}
	for _, v := range res.SupportedVersions {
		if v == ModernRevision {
			return Session{ProtocolVersion: ModernRevision}, nil
		}
	}
	return Session{}, fmt.Errorf("server/discover: server supports %v, this client speaks %s and the legacy handshake revisions",
		res.SupportedVersions, ModernRevision)
}

// ErrLegacyServer marks an era-probe outcome that identifies a legacy
// (handshake-era) server per the 2026-07-28 compatibility rules: any answer
// to server/discover that is not a recognized modern JSON-RPC error.
var ErrLegacyServer = errors.New("legacy MCP server")
