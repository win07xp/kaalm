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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// testChatRequest is the console's internal request shape
// (docs/src/gateways/api/internal-endpoints.md, POST /v1/test-chat).
type testChatRequest struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	UserID    string `json:"userId"`
	Content   string `json:"content"`
}

// handleTestChat delivers one console-authored message to one agent over the
// sync delivery pipeline and returns the agent's reply envelope verbatim.
// The console SAN check has already run (ConsolePaths). Message and reply
// bodies are never logged (the hard PII rule); the log line carries only the
// authenticated identity, the target, the outcome, and the duration.
func (s *Server) handleTestChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badRequest(w, "POST required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Config.MaxMessageBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, errorBody{
				Type:    errRequestTooLarge,
				Message: fmt.Sprintf("request body exceeds %d bytes", s.Config.MaxMessageBodyBytes)}, 0)
			return
		}
		badRequest(w, "reading request body: "+err.Error())
		return
	}
	var req testChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(w, "request body is not valid JSON")
		return
	}
	if req.Namespace == "" || req.Agent == "" || req.UserID == "" || req.Content == "" {
		badRequest(w, "namespace, agent, userId, and content are required")
		return
	}
	agent, ok := s.Store.AgentByName(r.Context(), req.Namespace, req.Agent)
	if !ok {
		writeError(w, http.StatusNotFound, errorBody{
			Type: errInvalidRequest, Message: "agent not found"}, 0)
		return
	}

	// The standard delivery envelope: channelType "console", a reserved
	// /console/... channel identifier that can never collide with a real
	// channel (every real one begins with /channels/), and a sessionId that
	// is always derived so repeated test chats from the same person to the
	// same agent share one conversation.
	channelID := fmt.Sprintf("/console/%s/%s", req.Namespace, req.Agent)
	env := MessageEnvelope{
		MessageID:   uuid.NewString(),
		ChannelType: "console",
		ChannelID:   channelID,
		UserID:      req.UserID,
		SessionID:   SessionID(channelID, req.UserID),
		Content:     req.Content,
		Attachments: []json.RawMessage{},
		Metadata:    map[string]any{},
	}

	tctx, endSpan := s.Tracing.Start(s.Tracing.Extract(r.Context(), r.Header), "channel.receive",
		trace.SpanKindServer,
		attribute.String("kaalm.channel_type", "console"),
		attribute.String("kaalm.namespace", req.Namespace),
		attribute.String("kaalm.agent", req.Agent),
		attribute.String("kaalm.message_id", env.MessageID))
	defer endSpan(nil)

	ctx, cancel := context.WithDeadline(tctx, time.Now().Add(s.Config.SyncDeliveryDeadline))
	defer cancel()

	start := time.Now()
	respBody, errType, err := s.wakeAndDeliver(ctx, "", agent, env)
	outcome := "delivered"
	if err != nil {
		outcome = errType
		if ctx.Err() != nil {
			outcome = errSyncDeadline
		}
	}
	slog.Info("test-chat",
		"namespace", req.Namespace, "agent", req.Agent, "userId", req.UserID,
		"outcome", outcome, "durationMs", time.Since(start).Milliseconds())
	if err != nil {
		spanError(ctx, errType)
		s.writeSyncError(w, ctx, agent.Namespace, errType, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}
