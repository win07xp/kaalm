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
	"time"
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
			stored := c.observations[path].failures[0].lastError
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

func TestChannelHealthStore_KeepsNewest256(t *testing.T) {
	c := NewChannelHealthStore(0)
	path := "/channels/team-a/hot"
	for i := range 300 {
		c.RecordFailure(path, healthReasonDispatchFailed, fmt.Sprint(i))
	}
	c.mu.Lock()
	list := c.observations[path].failures
	c.mu.Unlock()
	if len(list) != 256 {
		t.Fatalf("kept %d failures, want 256", len(list))
	}
	if first, last := list[0].lastError, list[len(list)-1].lastError; first != "44" || last != "299" {
		t.Fatalf("kept %q through %q, want the newest 256 (\"44\" through \"299\")", first, last)
	}
}

// A flood of failures inside the window cannot push out the path's newest
// success: anyone who knows a channel path can send badly authenticated
// requests, and the success must still be reported.
func TestChannelHealthStore_FailureFloodKeepsSuccess(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	c := NewChannelHealthStore(time.Minute)
	c.now = func() time.Time { return now }
	path := "/channels/team-a/flooded"
	c.RecordSuccess(path)
	for i := range 300 {
		now = t0.Add(time.Duration(i+1) * 100 * time.Millisecond)
		c.RecordFailure(path, healthReasonAuthFailed, fmt.Sprint("bad signature ", i))
	}
	entry, ok := c.Snapshot("team-a").Channels[path]
	if !ok {
		t.Fatalf("no entry for %s", path)
	}
	if entry.State != "success" || entry.Reason == nil || *entry.Reason != healthReasonWebhookReady {
		t.Fatalf("entry = state %q reason %v, want success %s", entry.State, entry.Reason, healthReasonWebhookReady)
	}
	if entry.LastError == nil || *entry.LastError != "bad signature 299" {
		t.Errorf("lastError = %v, want the newest failure's text", entry.LastError)
	}
}

func TestChannelHealthStore_SnapshotDropsAgedOutPaths(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	c := NewChannelHealthStore(time.Minute)
	c.now = func() time.Time { return now }

	// A deleted channel and a channel in another namespace never record again.
	c.RecordFailure("/channels/team-a/deleted", healthReasonDispatchFailed, "gone")
	c.RecordFailure("/channels/team-b/gone", healthReasonDispatchFailed, "gone")
	now = t0.Add(time.Second)
	c.RecordFailure("/channels/team-a/at-cutoff", healthReasonDispatchFailed, "edge")
	now = t0.Add(50 * time.Second)
	c.RecordSuccess("/channels/team-a/live")

	// The cutoff is t0+1s: an observation exactly at it has aged out.
	now = t0.Add(61 * time.Second)
	resp := c.Snapshot("team-a")
	for _, path := range []string{"/channels/team-a/deleted", "/channels/team-a/at-cutoff"} {
		if entry, ok := resp.Channels[path]; ok {
			t.Errorf("response has key %s (state %q), want none once all its observations aged out", path, entry.State)
		}
	}
	live := resp.Channels["/channels/team-a/live"]
	if live.State != "success" || live.Reason == nil || *live.Reason != healthReasonWebhookReady {
		t.Errorf("live channel = %+v, want success with reason WebhookReady", live)
	}

	// Snapshot drops aged-out paths in every namespace, without waiting for
	// the path to record again.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, path := range []string{"/channels/team-a/deleted", "/channels/team-b/gone", "/channels/team-a/at-cutoff"} {
		if _, ok := c.observations[path]; ok {
			t.Errorf("store still holds %s after all its observations aged out", path)
		}
	}
	if _, ok := c.observations["/channels/team-a/live"]; !ok {
		t.Error("store dropped the live channel")
	}
}

func TestChannelHealthStore_RecordDropsAgedOutPaths(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	c := NewChannelHealthStore(time.Minute)
	c.now = func() time.Time { return now }
	has := func(path string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.observations[path]
		return ok
	}

	// Nothing calls Snapshot here, as on a manager that does not poll the
	// gateway: records alone must bound memory.
	c.RecordFailure("/channels/team-a/old", healthReasonDispatchFailed, "x")
	now = t0.Add(30 * time.Second)
	c.RecordFailure("/channels/team-a/recent", healthReasonDispatchFailed, "x")
	now = t0.Add(61 * time.Second)
	c.RecordSuccess("/channels/team-a/other")
	if has("/channels/team-a/old") {
		t.Error("store still holds /channels/team-a/old after it aged out")
	}
	for _, path := range []string{"/channels/team-a/recent", "/channels/team-a/other"} {
		if !has(path) {
			t.Errorf("store dropped %s while it is inside the window", path)
		}
	}

	// recent has aged out by t0+91s, but less than one window has passed
	// since the last sweep. The request path sweeps the whole map at most
	// once per window, not on every record, so recent stays until then.
	now = t0.Add(91 * time.Second)
	c.RecordSuccess("/channels/team-a/next")
	if !has("/channels/team-a/recent") {
		t.Error("record swept the whole map again within one window")
	}
}
