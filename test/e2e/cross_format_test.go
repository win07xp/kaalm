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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// S24: a fallback edge that crosses formats, proven in both directions
// against the mock provider (docs/src/gateways/llm/fallback.md, Crossing
// formats).
var _ = Describe("Cross-format fallback (S24)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/cross-format.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/cross-format.yaml", "--ignore-not-found", "--wait=false")
		})

		By("all four providers reconcile Ready: the crossing edges pass rules 12 and 41")
		for _, name := range []string{"s24-anthropic-primary", "s24-openai-backup", "s24-openai-primary", "s24-anthropic-backup"} {
			Eventually(func() (bool, error) {
				return readyTrue("modelprovider", "", name)
			}, "60s", "3s").Should(BeTrue(), name)
		}
	})

	callerLogs := func(name string) string {
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", name, "{.status.phase}")
		}, "150s", "3s").Should(Equal("Succeeded"), name)
		logs, err := utils.Kubectl("logs", "-n", "e2e", name)
		Expect(err).NotTo(HaveOccurred())
		return logs
	}

	It("serves an Anthropic caller from the OpenAI backup, in the Anthropic shape", func() {
		logs := callerLogs("s24-anthropic-caller")
		Expect(logs).To(ContainSubstring("HTTP 200"))
		Expect(logs).To(ContainSubstring(`"type":"message"`))
		Expect(logs).To(ContainSubstring("ok from mock"))
		Expect(logs).To(ContainSubstring(`"model":"mock-gpt"`), "the model that served, mapped on the edge")
		Expect(logs).NotTo(ContainSubstring("chat.completion"), "no OpenAI shape reaches an Anthropic caller")
	})

	It("translates the stream event by event", func() {
		logs := callerLogs("s24-stream-caller")
		Expect(logs).To(ContainSubstring("HTTP 200"))
		for _, want := range []string{"event: message_start", "event: content_block_delta", "ok from mock",
			"event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
			Expect(logs).To(ContainSubstring(want), want)
		}
		Expect(logs).NotTo(ContainSubstring("[DONE]"))
	})

	It("serves an OpenAI caller from the Anthropic backup, supplying max_tokens from the catalog", func() {
		logs := callerLogs("s24-openai-caller")
		Expect(logs).To(ContainSubstring("HTTP 200"))
		Expect(logs).To(ContainSubstring(`"object":"chat.completion"`))
		Expect(logs).To(ContainSubstring("ok from mock"))
		Expect(logs).To(ContainSubstring(`"model":"claude-sonnet-4-6"`))
	})

	It("records spend on the provider that served, for the mapped model", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "s24-openai-backup",
				`{.status.budgetUsage[?(@.namespace=="e2e")].spentUSD}`)
		}, "120s", "5s").ShouldNot(BeEmpty())
		spent, _ := utils.ResourceField("modelprovider", "", "s24-openai-backup",
			`{.status.budgetUsage[?(@.namespace=="e2e")].spentUSD}`)
		Expect(spent).NotTo(Equal("0"))
		Expect(spent).NotTo(HavePrefix("0.0000"))
	})

	It("skips the crossing for a request it cannot translate and names the feature", func() {
		logs := callerLogs("s24-thinking-caller")
		Expect(logs).To(ContainSubstring("HTTP 502"))
		Expect(logs).To(ContainSubstring("provider_error"))
		By("the FallbackIneligible event on the primary names extended thinking")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "events", "-n", "default", "--field-selector", "reason=FallbackIneligible",
				"-o", "jsonpath={.items[*].message}")
		}, "60s", "3s").Should(ContainSubstring("extended thinking"))
	})
})
