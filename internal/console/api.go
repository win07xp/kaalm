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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// identityKey carries the authenticated Identity through a request context.
type identityKey struct{}

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}

// apiError is the console's JSON error envelope, mirroring the gateway's
// shape so API clients handle one format.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: apiErrorBody{Type: errType, Message: message}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// identity resolves the caller: a per-request bearer token first, then the
// session cookie. ok is false when neither authenticates.
func (s *Server) identity(r *http.Request) (Identity, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		id, err := s.Reviewer.Review(r.Context(), strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			return Identity{}, false
		}
		return id, true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return s.Sessions.Resolve(r.Context(), c.Value)
	}
	return Identity{}, false
}

// requireAPI authenticates the caller and gates namespace-scoped routes on
// CanView, attaching the Identity to the context.
func (s *Server) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.identity(r)
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized",
				"a valid bearer token or login session is required")
			return
		}
		if ns := r.PathValue("ns"); ns != "" {
			allowed, err := s.Gate.CanView(r.Context(), id, ns)
			if err != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "authorization check failed")
				return
			}
			if !allowed {
				writeAPIError(w, http.StatusForbidden, "access_denied",
					"you are not allowed to view this namespace")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	}
}

// apiNamespaces lists the namespaces this caller may view.
func (s *Server) apiNamespaces(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	all, err := s.Data.Namespaces(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "listing namespaces failed")
		return
	}
	visible := make([]string, 0, len(all))
	for _, ns := range all {
		allowed, err := s.Gate.CanView(r.Context(), id, ns)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "authorization check failed")
			return
		}
		if allowed {
			visible = append(visible, ns)
		}
	}
	writeJSON(w, map[string]any{"namespaces": visible})
}

func (s *Server) apiFleet(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Data.Fleet(r.Context(), r.PathValue("ns"))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "listing agents failed")
		return
	}
	writeJSON(w, map[string]any{"agents": rows})
}

func (s *Server) apiAgent(w http.ResponseWriter, r *http.Request) {
	detail, found, err := s.Data.Agent(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "reading the agent failed")
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "agent not found")
		return
	}
	writeJSON(w, detail)
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Data.Tasks(r.Context(), r.PathValue("ns"))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "listing tasks failed")
		return
	}
	writeJSON(w, map[string]any{"tasks": rows})
}

func (s *Server) apiChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Data.Channels(r.Context(), r.PathValue("ns"))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "listing channels failed")
		return
	}
	writeJSON(w, map[string]any{"channels": rows})
}

func (s *Server) apiSpend(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Data.Spend(r.Context(), r.PathValue("ns"))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "reading spend failed")
		return
	}
	writeJSON(w, map[string]any{"spend": rows})
}

// apiChat is the console's only write-shaped route. It is gated on CanChat
// (stricter than viewing) and relays the gateway's response verbatim. The
// message content is never logged.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	ns, agent := r.PathValue("ns"), r.PathValue("name")

	allowed, err := s.Gate.CanChat(r.Context(), id, ns)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_unavailable", "authorization check failed")
		return
	}
	if !allowed {
		writeAPIError(w, http.StatusForbidden, "access_denied",
			"test-chat requires permission to create channels in this namespace")
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "a non-empty content field is required")
		return
	}

	start := time.Now()
	status, body, err := s.Chat.Chat(r.Context(), ns, agent, id.Username, req.Content)
	if err != nil {
		slog.Error("test-chat gateway call failed",
			"namespace", ns, "agent", agent, "userId", id.Username, "err", err)
		writeAPIError(w, http.StatusBadGateway, "internal_unavailable", "the gateway could not be reached")
		return
	}
	slog.Info("test-chat",
		"namespace", ns, "agent", agent, "userId", id.Username,
		"status", status, "durationMs", time.Since(start).Milliseconds())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
