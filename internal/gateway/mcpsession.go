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

package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// MCP session ownership is a stateless binding: the broker never reveals an
// upstream Mcp-Session-Id. The caller sees the upstream id bound to its own
// identity under an HMAC keyed by gateway-shared material, so any replica
// verifies that one workload cannot resume another's session, with no shared
// session table. See docs/src/gateways/tool-plane.md (The Broker).

// callerIdentity is the stable identity string the session HMAC binds to:
// namespace/Kind/name for workloads, ns/<namespace> for the bearer tier
// (which carries no workload identity).
func callerIdentity(c *caller) string {
	if c.Workload != nil {
		return c.Namespace + "/" + string(c.Workload.Kind) + "/" + c.Workload.Name
	}
	return "ns/" + c.Namespace
}

// wrapSessionID binds an upstream session id to a caller identity:
// base64url(upstreamID) "." base64url(HMAC-SHA256(key, upstreamID 0x00 identity)).
func wrapSessionID(key []byte, upstreamID, identity string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(upstreamID)) + "." +
		base64.RawURLEncoding.EncodeToString(sessionMAC(key, upstreamID, identity))
}

// unwrapSessionID verifies a wrapped id against the presenting caller's
// identity and returns the upstream id. ok is false on any malformed input
// or MAC mismatch; the caller must reject before forwarding anything.
func unwrapSessionID(key []byte, wrapped, identity string) (string, bool) {
	idPart, macPart, found := strings.Cut(wrapped, ".")
	if !found {
		return "", false
	}
	upstreamID, err := base64.RawURLEncoding.DecodeString(idPart)
	if err != nil {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil {
		return "", false
	}
	if !hmac.Equal(mac, sessionMAC(key, string(upstreamID), identity)) {
		return "", false
	}
	return string(upstreamID), true
}

func sessionMAC(key []byte, upstreamID, identity string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(upstreamID))
	h.Write([]byte{0})
	h.Write([]byte(identity))
	return h.Sum(nil)
}
