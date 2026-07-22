//go:build e2e

package e2e

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// S4 (fallback walk) and S10 (budget exhaustion) against the mock provider.
// Both ride the mock's path-prefix behaviors: /fail (503) and /ok drive the
// fallback walk; /bigusage inflates usage to exhaust a low budget.
var _ = Describe("Fallback and budget", Ordered, func() {
	BeforeAll(func() {
		By("ensuring the mock provider is deployed")
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())

		// The ModelProviders and AgentClass here are cluster-scoped; AfterSuite
		// only deletes the e2e namespace, so clean them up explicitly.
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/mock-fallback.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/mock-budget.yaml", "--ignore-not-found", "--wait=false")
		})
	})

	It("S4: walks to the fallback provider when the primary fails", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mock-fallback.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("both providers reconcile Ready")
		for _, name := range []string{"s4-primary", "s4-fallback"} {
			Eventually(func() (bool, error) {
				return readyTrue("modelprovider", "", name)
			}, "60s", "3s").Should(BeTrue(), name)
		}

		By("the caller (routed at the failing primary) gets a 200 from the fallback")
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s4-caller", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "e2e", "s4-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 200"))
		Expect(logs).To(ContainSubstring("ok from mock"))
	})

	It("S10: exhausts the budget, blocks with 429, and degrades the Agent", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mock-budget.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("the provider reconciles Ready")
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "s10-budget")
		}, "60s", "3s").Should(BeTrue())

		By("the caller drives spend past the ceiling and is blocked with 429 budget_exhausted")
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s10-caller", "{.status.phase}")
		}, "150s", "5s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "e2e", "s10-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 429"))
		Expect(logs).To(ContainSubstring("budget_exhausted"))
		Expect(strings.ToLower(logs)).To(ContainSubstring("retry-after"))

		By("the ModelProvider reports the namespace Blocked")
		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "s10-budget",
				`{.status.budgetUsage[?(@.namespace=="e2e")].state}`)
		}, "90s", "5s").Should(Equal("Blocked"))

		By("the Agent carries the Degraded/BudgetExhausted condition, phase preserved")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s10-agent",
				`{.status.conditions[?(@.type=="Degraded")].reason}`)
		}, "90s", "5s").Should(Equal("BudgetExhausted"))
	})
})
