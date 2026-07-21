/*
Copyright 2026.

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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// MessageEnvelope is the normalized message delivered to agents on
// POST /v1/message. See docs/src/gateways/api/agent-endpoints.md.
type MessageEnvelope struct {
	MessageID   string            `json:"messageId"`
	ChannelType string            `json:"channelType"`
	ChannelID   string            `json:"channelId"`
	UserID      string            `json:"userId"`
	SessionID   string            `json:"sessionId,omitempty"`
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments"`
	Metadata    map[string]any    `json:"metadata"`
}

// ResponseEnvelope is the agent's reply shape. content is required; the
// optional fields pass through unvalidated.
type ResponseEnvelope struct {
	Content     *string           `json:"content"`
	Attachments []json.RawMessage `json:"attachments,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
}

// SessionID derives the deterministic session identity:
// UUIDv5(SessionNamespaceUUID, channelId + ":" + userId). The namespace
// constant is published API and must never change after v1.
func SessionID(channelID, userID string) string {
	ns := uuid.MustParse(agentryv1alpha1.SessionNamespaceUUID)
	return uuid.NewSHA1(ns, []byte(channelID+":"+userID)).String()
}

// normalizeError distinguishes a malformed body (400) from success.
type normalizeError struct{ msg string }

func (e *normalizeError) Error() string { return e.msg }

// normalize builds the envelope from the inbound webhook request per the
// channel's extraction config. body is the raw inbound bytes (already read
// and size-capped).
func normalize(channel *agentryv1alpha1.AgentChannel, r *http.Request, body []byte) (MessageEnvelope, error) {
	env := MessageEnvelope{
		MessageID:   uuid.NewString(),
		ChannelType: "webhook",
		ChannelID:   channel.Spec.Webhook.Path,
		Attachments: []json.RawMessage{},
		Metadata:    map[string]any{},
	}

	needsJSON := extractorUsesBody(channel.Spec.Webhook.UserID) || extractorUsesBody(channel.Spec.Webhook.Content)
	var parsedBody map[string]any
	if needsJSON {
		if err := json.Unmarshal(body, &parsedBody); err != nil {
			return env, &normalizeError{"fromBody extraction is configured but the request body is not valid JSON"}
		}
	}

	env.UserID = extract(channel.Spec.Webhook.UserID, r, parsedBody)

	if isExtractorConfigured(channel.Spec.Webhook.Content) {
		env.Content = extract(channel.Spec.Webhook.Content, r, parsedBody)
	} else {
		// Raw-body fallback: the inbound body, JSON-encoded as a string.
		// Requires valid UTF-8; binary senders must configure content.
		if !utf8.Valid(body) {
			return env, &normalizeError{fmt.Sprintf(
				"raw-body content requires valid UTF-8; invalid byte at offset %d", utf8ErrorOffset(body))}
		}
		encoded, err := json.Marshal(string(body))
		if err != nil {
			return env, &normalizeError{err.Error()}
		}
		env.Content = string(encoded)
	}

	if channel.Spec.Session.Enabled {
		env.SessionID = SessionID(env.ChannelID, env.UserID)
	}
	return env, nil
}

func isExtractorConfigured(e agentryv1alpha1.ChannelExtractor) bool {
	return e.FromHeader != nil || e.FromBody != nil
}

func extractorUsesBody(e agentryv1alpha1.ChannelExtractor) bool {
	return e.FromBody != nil
}

// extract resolves an extractor against the request, falling back to the
// configured fallback (empty string if omitted).
func extract(e agentryv1alpha1.ChannelExtractor, r *http.Request, body map[string]any) string {
	var value string
	switch {
	case e.FromHeader != nil:
		value = r.Header.Get(*e.FromHeader)
	case e.FromBody != nil:
		value = dottedLookup(body, *e.FromBody)
	}
	if value == "" && e.Fallback != nil {
		return *e.Fallback
	}
	return value
}

// dottedLookup walks a dotted path (for example "user.id") into a JSON body.
func dottedLookup(body map[string]any, path string) string {
	current := any(body)
	for _, part := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = m[part]
		if !ok {
			return ""
		}
	}
	switch v := current.(type) {
	case string:
		return v
	case float64, bool:
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func utf8ErrorOffset(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}
