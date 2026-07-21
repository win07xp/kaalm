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
	"testing"
)

func TestChannelsHealthEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	// A failure-only channel and a success channel exercise both Snapshot arms.
	h.server.ChannelHealth.RecordFailure("/channels/team-a/failing", healthReasonAuthFailed, "bad token")
	h.server.ChannelHealth.RecordSuccess("/channels/team-a/ok")

	controllerCert := h.ca.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")
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
