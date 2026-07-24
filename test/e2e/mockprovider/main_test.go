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

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBehaviorFor(t *testing.T) {
	cases := []struct {
		path       string
		wantStatus int
		wantUsage  bool // expect non-zero token counts
	}{
		{"/ok/v1/chat/completions", http.StatusOK, true},
		{"/v1/chat/completions", http.StatusOK, true}, // default is ok
		{"/fail/v1/chat/completions", http.StatusServiceUnavailable, false},
		{"/bigusage/v1/chat/completions", http.StatusOK, true},
	}
	for _, c := range cases {
		status, in, out := behaviorFor(c.path)
		if status != c.wantStatus {
			t.Errorf("%s: status=%d want %d", c.path, status, c.wantStatus)
		}
		if got := in > 0 && out > 0; got != c.wantUsage {
			t.Errorf("%s: nonzero usage=%v want %v (in=%d out=%d)", c.path, got, c.wantUsage, in, out)
		}
	}
	// bigusage must dwarf ok so budget tests cross the ceiling in few calls.
	_, bigIn, _ := behaviorFor("/bigusage/x")
	_, okIn, _ := behaviorFor("/ok/x")
	if bigIn <= okIn {
		t.Errorf("bigusage input tokens %d not greater than ok %d", bigIn, okIn)
	}
}

func TestChatSuccessCarriesUsage(t *testing.T) {
	m := &mock{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ok/v1/chat/completions",
		strings.NewReader(`{"model":"mock-model","messages":[]}`))
	m.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var body struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Usage.PromptTokens == 0 || body.Usage.CompletionTokens == 0 {
		t.Errorf("usage fields must be non-zero, got %+v", body.Usage)
	}
}

func TestChatFailReturns503(t *testing.T) {
	m := &mock{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/fail/v1/chat/completions", strings.NewReader(`{}`))
	m.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

func TestCallbackRecordedAndIntrospected(t *testing.T) {
	m := &mock{}
	post := httptest.NewRecorder()
	m.handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/callback",
		strings.NewReader(`{"requestId":"abc"}`)))
	if post.Code != http.StatusOK {
		t.Fatalf("callback status=%d want 200", post.Code)
	}

	get := httptest.NewRecorder()
	m.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/introspect/callbacks", nil))
	var recorded []recordedCallback
	if err := json.Unmarshal(get.Body.Bytes(), &recorded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(recorded) != 1 || !strings.Contains(recorded[0].Body, "abc") {
		t.Errorf("callback not recorded: %+v", recorded)
	}
}
