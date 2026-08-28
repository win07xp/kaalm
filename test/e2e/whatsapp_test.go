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
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

const s23Number = "106540352242922"

type whatsAppSend struct {
	Path         string          `json:"path"`
	Event        json.RawMessage `json:"event"`
	BadSignature bool            `json:"badSignature,omitempty"`
}

func s23Event(number string, messages []string, statuses []string) json.RawMessage {
	joinRaw := func(items []string) string {
		out := "["
		for i, it := range items {
			if i > 0 {
				out += ","
			}
			out += it
		}
		return out + "]"
	}
	return json.RawMessage(`{"object":"whatsapp_business_account","entry":[{"id":"102290129340398","changes":[{"field":"messages","value":{` +
		`"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550001234","phone_number_id":"` + number + `"},` +
		`"contacts":[{"profile":{"name":"Dev"},"wa_id":"15551234567"}],"messages":` + joinRaw(messages) +
		`,"statuses":` + joinRaw(statuses) + `}}]}]}`)
}

func s23Text(id, body string) string {
	return `{"from":"15551234567","id":"` + id + `","timestamp":"1756100000","type":"text","text":{"body":"` + body + `"}}`
}

var _ = Describe("WhatsApp channel (S23)", Ordered, func() {
	var (
		mockURL string
		stop    func()
	)

	relay := func(method, path string, body []byte) discordAnswer {
		var out discordAnswer
		Eventually(func() error {
			req, err := http.NewRequest(method, mockURL+path, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Errorf("mock %s answered %d: %s", path, resp.StatusCode, raw)
			}
			return json.Unmarshal(raw, &out)
		}, "60s", "3s").Should(Succeed())
		return out
	}
	send := func(req whatsAppSend) discordAnswer {
		raw, _ := json.Marshal(req)
		return relay(http.MethodPost, "/send", raw)
	}
	verify := func(token, challenge string) discordAnswer {
		q := url.Values{"path": {"/channels/e2e/s23-channel"}, "token": {token}, "challenge": {challenge}}
		return relay(http.MethodGet, "/verify?"+q.Encode(), nil)
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
	platformConnected := func() string {
		status, _ := utils.ResourceField("agentchannel", "e2e", "s23-channel",
			`{.status.conditions[?(@.type=="PlatformConnected")].status}`)
		reason, _ := utils.ResourceField("agentchannel", "e2e", "s23-channel",
			`{.status.conditions[?(@.type=="PlatformConnected")].reason}`)
		return status + "/" + reason
	}

	BeforeAll(func() {
		_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/whatsapp.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-whatsapp", "120s")).To(Succeed())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s23-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the WhatsApp channel reconciles to Ready with its credential Role scoped to the Secret")
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s23-channel",
				`{.status.conditions[?(@.type=="Ready")].status}`)
		}, "90s", "3s").Should(Equal("True"))
		names, err := utils.ResourceField("role", "e2e", "kaalm-channel-s23-channel-creds", "{.rules[0].resourceNames}")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(ContainSubstring("s23-whatsapp-creds"))

		port, stopFn, err := utils.PortForward("e2e", "mock-whatsapp", "8080")
		Expect(err).NotTo(HaveOccurred())
		stop = stopFn
		mockURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	})

	AfterAll(func() {
		if stop != nil {
			stop()
		}
	})

	It("echoes the verification challenge for the right token and refuses the wrong one", func() {
		ok := verify("e2e-verify-token", "1158201444")
		Expect(ok.Status).To(Equal(200))
		// The mock relays the gateway's body as JSON; a digits-only challenge
		// is a JSON number, so the echo arrives unquoted.
		Expect(strings.Trim(string(ok.Body), `"`)).To(Equal("1158201444"))

		bad := verify("guess", "1158201444")
		Expect(bad.Status).To(Equal(403))
	})

	It("rejects a badly signed event and shows it on PlatformConnected", func() {
		bad := send(whatsAppSend{Path: "/channels/e2e/s23-channel",
			Event: s23Event(s23Number, []string{s23Text("wamid.bad", "hi")}, nil), BadSignature: true})
		Expect(bad.Status).To(Equal(401))
		Eventually(platformConnected, "150s", "5s").Should(Equal("False/WebhookAuthFailed"))
	})

	It("acknowledges a signed text event at once and answers through the Graph API", func() {
		before := len(replies())
		ack := send(whatsAppSend{Path: "/channels/e2e/s23-channel",
			Event: s23Event(s23Number, []string{s23Text("wamid.1", "Where is my order?")}, nil)})
		Expect(ack.Status).To(Equal(200))

		By("the reply is a text message to the sender from the channel's number, with the access token")
		var got discordRecordedReply
		Eventually(func() int {
			all := replies()
			if len(all) > before {
				got = all[before]
			}
			return len(all)
		}, "120s", "3s").Should(BeNumerically(">", before))
		Expect(got.Method).To(Equal("POST"))
		Expect(got.Path).To(Equal("/" + s23Number + "/messages"))
		Expect(got.Authorization).To(Equal("Bearer e2e-graph-access-token"))
		var body struct {
			To   string `json:"to"`
			Type string `json:"type"`
			Text struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		Expect(json.Unmarshal(got.Body, &body)).To(Succeed())
		Expect(body.To).To(Equal("15551234567"))
		Expect(body.Type).To(Equal("text"))
		Expect(body.Text.Body).NotTo(BeEmpty())
		Expect(body.Text.Body).NotTo(HavePrefix("delivery_failed"))

		By("a delivered reply flips PlatformConnected to True")
		Eventually(platformConnected, "150s", "5s").Should(Equal("True/WebhookReady"))
	})

	It("acknowledges status events and other numbers' messages without delivering them", func() {
		before := len(replies())
		status := `{"id":"wamid.reply","status":"delivered","timestamp":"1756100001","recipient_id":"15551234567"}`
		ack := send(whatsAppSend{Path: "/channels/e2e/s23-channel", Event: s23Event(s23Number, nil, []string{status})})
		Expect(ack.Status).To(Equal(200))
		other := send(whatsAppSend{Path: "/channels/e2e/s23-channel",
			Event: s23Event("999000111222333", []string{s23Text("wamid.x", "elsewhere")}, nil)})
		Expect(other.Status).To(Equal(200))
		Consistently(func() int { return len(replies()) }, "10s", "2s").Should(Equal(before))
	})
})
