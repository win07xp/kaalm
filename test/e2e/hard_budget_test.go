//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// mockRequestCount reads the mock provider's per-prefix chat call counter.
func mockRequestCount(port int, prefix string) (int, error) {
	status, body, err := utils.GetWithBearer(fmt.Sprintf("https://127.0.0.1:%d/introspect/requests", port), "")
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("introspect status %d: %s", status, body)
	}
	counts := map[string]int{}
	if err := json.Unmarshal([]byte(body), &counts); err != nil {
		return 0, fmt.Errorf("decoding %q: %w", body, err)
	}
	return counts[prefix], nil
}

var _ = Describe("Hard budget cap (S17)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		// The providers are cluster-scoped; the namespace teardown would
		// miss them (same pattern as the fallback/budget spec).
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s17-recheck.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s17-hard-budget.yaml", "--ignore-not-found", "--wait=false")
		})
	})

	It("reconciles the hard and soft providers to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/s17-hard-budget.yaml")
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"s17-hard", "s17-soft"} {
			Eventually(func() (bool, error) {
				return readyTrue("modelprovider", "", name)
			}, "60s", "3s").Should(BeTrue(), name)
		}
	})

	It("blocks the hard provider at the ceiling and names what fired", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s17-hard-caller", "{.status.phase}")
		}, "150s", "5s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "e2e", "s17-hard-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 429"))
		Expect(logs).To(ContainSubstring("budget_exhausted"))
		// The hard design's ceiling attribution: the message says which
		// ceiling fired rather than a generic phrase.
		Expect(logs).To(ContainSubstring("namespace budget exhausted: e2e"))
		Expect(strings.ToLower(logs)).To(ContainSubstring("retry-after"))

		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "s17-hard",
				`{.status.budgetUsage[?(@.namespace=="e2e")].state}`)
		}, "90s", "5s").Should(Equal("Blocked"))
	})

	It("rejects post-exhaustion requests with no upstream call", func() {
		port, stop, err := utils.PortForward("e2e", "mock-provider", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		// The counter's absolute value depends on history (on a re-run
		// against a standing cluster, the gateway ledger already blocks
		// while the namespace-scoped mock restarted with fresh counters),
		// so the proof is the DELTA across the recheck, not the level.
		before, err := mockRequestCount(port, "/bigusage/s17h")
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "mock chat calls for /bigusage/s17h before recheck: %d\n", before)

		// Delete first: a leftover completed probe pod would not restart on
		// a bare apply.
		_, err = utils.Kubectl("delete", "-f", "test/e2e/testdata/s17-recheck.yaml", "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/s17-recheck.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s17-hard-recheck", "{.status.phase}")
		}, "120s", "5s").Should(Equal("Succeeded"))
		logs, err := utils.Kubectl("logs", "-n", "e2e", "s17-hard-recheck")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("BOTH REJECTED"))
		Expect(logs).To(ContainSubstring("budget_exhausted"))

		after, err := mockRequestCount(port, "/bigusage/s17h")
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before),
			"a blocked hard provider must reject without forwarding: the mock's counter may not move")
	})

	It("keeps the soft twin on today's behavior", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s17-soft-caller", "{.status.phase}")
		}, "150s", "5s").Should(Equal("Succeeded"))
		logs, err := utils.Kubectl("logs", "-n", "e2e", "s17-soft-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 429"))
		Expect(logs).To(ContainSubstring("budget_exhausted"))
	})
})
