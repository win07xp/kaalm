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
	"net/http"
	"strconv"
)

// errorBody is the structured envelope every gateway error carries. Callers
// branch on Type, never on Message. See docs/src/gateways/api/errors.md.
type errorBody struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Provider  string `json:"provider,omitempty"`
	Retryable bool   `json:"retryable"`
}

// Stable error.type values used by the Phase 5 surface.
const (
	errInvalidRequest      = "invalid_request"
	errUnauthorized        = "unauthorized"
	errAccessDenied        = "access_denied"
	errInvalidCert         = "invalid_cert"
	errRequestTooLarge     = "request_too_large"
	errInternalUnavailable = "internal_unavailable"
	errProviderError       = "provider_error"
	errProviderUnavailable = "provider_unavailable"
	errProviderTimeout     = "provider_timeout"
)

// writeError emits the envelope with the given status. retryAfter > 0 adds a
// Retry-After header in delta-seconds.
func writeError(w http.ResponseWriter, status int, body errorBody, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]errorBody{"error": body})
}

func unauthorized(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusUnauthorized, errorBody{Type: errUnauthorized, Message: msg}, 0)
}

func forbidden(w http.ResponseWriter, errType, msg string) {
	writeError(w, http.StatusForbidden, errorBody{Type: errType, Message: msg}, 0)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, errorBody{Type: errInvalidRequest, Message: msg}, 0)
}
