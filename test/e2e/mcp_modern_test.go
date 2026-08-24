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

// The dual-era tool plane (docs/src/gateways/tool-plane.md, Protocol
// Revisions): a 2026-07-28-only mock beside S18's legacy mock, the probe
// negotiating each era, and the broker enforcing the modern posture. S18's
// own spec keeps proving the legacy posture on the same cluster.
var _ = Describe("MCP 2026-07-28 revision (S18, dual-era)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mcp-modern.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/mcp-modern-caller.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/mcp-modern.yaml", "--ignore-not-found", "--wait=false")
		})
	})

	It("negotiates the stateless revision: Healthy with status.mcpRevision 2026-07-28", func() {
		Expect(utils.WaitRollout("e2e", "mock-mcp-modern", "120s")).To(Succeed())
		Eventually(func() (string, error) {
			return utils.ResourceField("toolprovider", "", "search-tools-modern",
				`{.status.conditions[?(@.type=="Healthy")].status}`)
		}, "120s", "5s").Should(Equal("True"))
		Eventually(func() (string, error) {
			return utils.ResourceField("toolprovider", "", "search-tools-modern", "{.status.mcpRevision}")
		}, "60s", "3s").Should(Equal("2026-07-28"))
	})

	It("brokers modern traffic: validated headers, filtered private list, denials intact", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "mcp-modern-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mcp-modern-caller.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "mcp-modern-caller", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "e2e", "mcp-modern-caller")
		Expect(err).NotTo(HaveOccurred())

		By("server/discover is brokered and answers the supported versions")
		discover := markerLine(logs, "discover")
		Expect(discover).To(ContainSubstring("HTTP 200"), logs)
		Expect(discover).To(ContainSubstring("2026-07-28"))

		By("the stateless tools/list is filtered to the grant and rewritten cacheScope: private")
		list := markerLine(logs, "tools-list")
		Expect(list).To(ContainSubstring("HTTP 200"), logs)
		Expect(list).To(ContainSubstring("web_search"))
		Expect(list).NotTo(ContainSubstring("fetch_page"),
			"the model must never see a tool the workload cannot call")
		Expect(list).To(ContainSubstring(`"cacheScope":"private"`),
			"a per-caller-filtered list must never be shared-cacheable")

		By("a granted call with consistent headers round-trips, headers forwarded and validated upstream")
		granted := markerLine(logs, "call-granted")
		Expect(granted).To(ContainSubstring("HTTP 200"), logs)
		Expect(granted).To(ContainSubstring("called web_search"))

		By("a mismatched Mcp-Name is rejected by the broker with the revision's HeaderMismatch")
		mismatch := markerLine(logs, "header-mismatch")
		Expect(mismatch).To(ContainSubstring("HTTP 400"), logs)
		Expect(mismatch).To(ContainSubstring("-32020"))

		By("the grant chain still denies an ungranted tool, headers consistent or not")
		ungranted := markerLine(logs, "call-ungranted")
		Expect(ungranted).To(ContainSubstring("HTTP 403"), logs)
		Expect(ungranted).To(ContainSubstring("tool_denied"))
	})
})
