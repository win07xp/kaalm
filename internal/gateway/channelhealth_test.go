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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/win07xp/kaalm/internal/eventrec"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestChannelsHealthEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	// A failure-only channel and a success channel exercise both Snapshot arms.
	h.server.ChannelHealth.RecordFailure("/channels/team-a/failing", healthReasonAuthFailed, "bad token")
	h.server.ChannelHealth.RecordSuccess("/channels/team-a/ok")

	controllerCert := h.ca.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kaalm-system"}}
	resp, err := h.client(&controllerCert).Get(h.url("/v1/channels/health?namespace=team-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("channels health = %d", resp.StatusCode)
	}
	var payload struct {
		WindowSeconds int `json:"windowSeconds"`
		Channels      map[string]struct {
			State     string  `json:"state"`
			Reason    *string `json:"reason"`
			LastError *string `json:"lastError"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channels["/channels/team-a/failing"].State != "failure" {
		t.Errorf("failing channel state = %q", payload.Channels["/channels/team-a/failing"].State)
	}
	// A healthy channel reports the WebhookReady reason (the success indicator).
	okReason := payload.Channels["/channels/team-a/ok"].Reason
	if okReason == nil || *okReason != healthReasonWebhookReady {
		t.Errorf("ok channel reason = %v", okReason)
	}
}

func TestChannelHealthStore_CutsLongFailureText(t *testing.T) {
	const marker = "... (truncated)"
	cases := []struct {
		name    string
		text    string
		wantCut bool
	}{
		{name: "long ascii", text: strings.Repeat("x", 40000), wantCut: true},
		{name: "exactly the bound", text: strings.Repeat("x", eventrec.MaxNoteBytes)},
		{name: "one byte over", text: strings.Repeat("x", eventrec.MaxNoteBytes+1), wantCut: true},
		{name: "short", text: "bad token"},
		{name: "long multibyte", text: strings.Repeat("é", 2000), wantCut: true},
	}
	c := NewChannelHealthStore(0)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf("/channels/team-a/case-%d", i)
			c.RecordFailure(path, healthReasonDispatchFailed, tc.text)

			c.mu.Lock()
			stored := c.observations[path][0].lastError
			c.mu.Unlock()
			served := *c.Snapshot("team-a").Channels[path].LastError

			for what, got := range map[string]string{"stored": stored, "served": served} {
				if !tc.wantCut {
					if got != tc.text {
						t.Fatalf("%s text changed: got %d bytes, want %d kept whole", what, len(got), len(tc.text))
					}
					continue
				}
				if len(got) > eventrec.MaxNoteBytes || !strings.HasSuffix(got, marker) {
					t.Fatalf("%s text is %d bytes, want at most %d ending in the marker", what, len(got), eventrec.MaxNoteBytes)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("%s text splits a UTF-8 character", what)
				}
			}
		})
	}

	t.Run("success arm", func(t *testing.T) {
		path := "/channels/team-a/recovered"
		c.RecordFailure(path, healthReasonDispatchFailed, strings.Repeat("x", 40000))
		c.RecordSuccess(path)
		entry := c.Snapshot("team-a").Channels[path]
		if entry.State != "success" || entry.LastError == nil {
			t.Fatalf("got state %q, lastError %v; want success with the earlier failure text", entry.State, entry.LastError)
		}
		if len(*entry.LastError) > eventrec.MaxNoteBytes {
			t.Fatalf("lastError is %d bytes, want at most %d", len(*entry.LastError), eventrec.MaxNoteBytes)
		}
	})
}
