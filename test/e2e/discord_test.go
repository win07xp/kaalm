//go:build e2e

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

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// discordSend asks the mock to sign and deliver an interaction and returns the
// gateway's answer.
type discordSend struct {
	Path                   string          `json:"path"`
	Interaction            json.RawMessage `json:"interaction"`
	BadSignature           bool            `json:"badSignature,omitempty"`
	TimestampOffsetSeconds int64           `json:"timestampOffsetSeconds,omitempty"`
}

type discordAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type discordRecordedReply struct {
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Authorization string          `json:"authorization"`
	Body          json.RawMessage `json:"body"`
}

func s22Command(guild, message string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"id": "1290000000000000001", "application_id": "1230000000000000000", "type": 2,
		"token": "e2e-interaction-token", "guild_id": guild, "channel_id": "987654321098765432",
		"member": map[string]any{"user": map[string]any{"id": "555555555555555555", "username": "dev"}},
		"data":   map[string]any{"name": "ask", "options": []map[string]any{{"name": "message", "type": 3, "value": message}}},
		"locale": "en-US",
	})
	return raw
}

var _ = Describe("Discord channel (S22)", Ordered, func() {
	var (
		mockURL string
		stop    func()
	)

	send := func(req discordSend) discordAnswer {
		raw, _ := json.Marshal(req)
		var out discordAnswer
		Eventually(func() error {
			resp, err := http.Post(mockURL+"/send", "application/json", bytes.NewReader(raw))
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Errorf("mock /send answered %d: %s", resp.StatusCode, body)
			}
			return json.Unmarshal(body, &out)
		}, "60s", "3s").Should(Succeed())
		return out
	}

	replies := func() []discordRecordedReply {
		resp, err := http.Get(mockURL + "/introspect/replies")
		if err != nil {
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		var out []discordRecordedReply
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	platformConnected := func() (string, string) {
		status, _ := utils.ResourceField("agentchannel", "e2e", "s22-channel",
			`{.status.conditions[?(@.type=="PlatformConnected")].status}`)
		reason, _ := utils.ResourceField("agentchannel", "e2e", "s22-channel",
			`{.status.conditions[?(@.type=="PlatformConnected")].reason}`)
		return status, reason
	}

	BeforeAll(func() {
		_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/discord.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-discord", "120s")).To(Succeed())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s22-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the Discord channel reconciles to Ready with its credential Role scoped to the Secret")
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s22-channel",
				`{.status.conditions[?(@.type=="Ready")].status}`)
		}, "90s", "3s").Should(Equal("True"))
		names, err := utils.ResourceField("role", "e2e", "kaalm-channel-s22-channel-creds", "{.rules[0].resourceNames}")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(ContainSubstring("s22-discord-creds"))

		port, stopFn, err := utils.PortForward("e2e", "mock-discord", "8080")
		Expect(err).NotTo(HaveOccurred())
		stop = stopFn
		mockURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	})

	AfterAll(func() {
		if stop != nil {
			stop()
		}
	})

	It("answers the verification PING with PONG and rejects a badly signed one", func() {
		ping := json.RawMessage(`{"id":"1","application_id":"1230000000000000000","type":1,"token":"t"}`)

		By("a badly signed request is 401, the way Discord's save-time check requires")
		bad := send(discordSend{Path: "/channels/e2e/s22-channel", Interaction: ping, BadSignature: true})
		Expect(bad.Status).To(Equal(401))

		By("a stale timestamp is 401 too")
		stale := send(discordSend{Path: "/channels/e2e/s22-channel", Interaction: ping, TimestampOffsetSeconds: -900})
		Expect(stale.Status).To(Equal(401))

		By("the auth failures show on PlatformConnected before any success")
		Eventually(func() string {
			status, reason := platformConnected()
			return status + "/" + reason
		}, "150s", "5s").Should(Equal("False/WebhookAuthFailed"))

		By("a valid PING gets PONG")
		ok := send(discordSend{Path: "/channels/e2e/s22-channel", Interaction: ping})
		Expect(ok.Status).To(Equal(200))
		Expect(string(ok.Body)).To(MatchJSON(`{"type":1}`))
	})

	It("acknowledges a slash command at once and delivers the reply through the follow-up webhook", func() {
		before := len(replies())
		ack := send(discordSend{Path: "/channels/e2e/s22-channel", Interaction: s22Command("123456789012345678", "Where is my order?")})
		Expect(ack.Status).To(Equal(200))
		Expect(string(ack.Body)).To(MatchJSON(`{"type":5}`))

		By("the reply edits the deferred message through the interaction's webhook, with no bot token")
		var got discordRecordedReply
		Eventually(func() int {
			all := replies()
			if len(all) > before {
				got = all[before]
			}
			return len(all)
		}, "120s", "3s").Should(BeNumerically(">", before))
		Expect(got.Method).To(Equal("PATCH"))
		Expect(got.Path).To(Equal("/api/v10/webhooks/1230000000000000000/e2e-interaction-token/messages/@original"))
		Expect(got.Authorization).To(BeEmpty())
		var body struct {
			Content string `json:"content"`
		}
		Expect(json.Unmarshal(got.Body, &body)).To(Succeed())
		Expect(body.Content).NotTo(BeEmpty())
		Expect(body.Content).NotTo(HavePrefix("delivery_failed"))

		By("a delivered command flips PlatformConnected to True")
		Eventually(func() string {
			status, reason := platformConnected()
			return status + "/" + reason
		}, "150s", "5s").Should(Equal("True/WebhookReady"))
	})

	It("refuses an interaction from another guild with an ephemeral message and never reaches the agent", func() {
		before := len(replies())
		ans := send(discordSend{Path: "/channels/e2e/s22-channel", Interaction: s22Command("999999999999999999", "hi")})
		Expect(ans.Status).To(Equal(200))
		var body struct {
			Type int `json:"type"`
			Data struct {
				Content string `json:"content"`
				Flags   int    `json:"flags"`
			} `json:"data"`
		}
		Expect(json.Unmarshal(ans.Body, &body)).To(Succeed())
		Expect(body.Type).To(Equal(4))
		Expect(body.Data.Flags).To(Equal(64))
		Expect(strings.ToLower(body.Data.Content)).To(ContainSubstring("not available"))
		Consistently(func() int { return len(replies()) }, "10s", "2s").Should(Equal(before))
	})
})
