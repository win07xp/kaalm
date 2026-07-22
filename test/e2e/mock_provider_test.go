//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// Mock provider integration: proves the gateway forwards an authenticated LLM
// request to an in-cluster upstream over TLS and gets usage back. This is the
// infra the S4/S10 (#8) and S12/S15 (#11) scenarios build on.
var _ = Describe("Mock LLM provider", Ordered, func() {
	It("deploys the mock and reconciles its ModelProvider to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("the mock provider Deployment rolls out")
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())

		By("the ModelProvider becomes Ready (probe disabled; credentials resolve)")
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "e2e-mock")
		}, "60s", "3s").Should(BeTrue())
	})

	It("forwards a token-authenticated LLM call to the mock and returns usage", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/llm-caller.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("the caller Pod runs to completion")
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "llm-caller", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Succeeded"))

		By("the gateway returned 200 with the mock's completion")
		logs, err := utils.Kubectl("logs", "-n", "e2e", "llm-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 200"))
		Expect(logs).To(ContainSubstring("ok from mock"))
	})
})
