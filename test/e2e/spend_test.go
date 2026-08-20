//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// Per-workload spend (#100): two agent-identity callers with distinct call
// counts produce two distinct rows on the console's spend surface, the
// unattributed-vs-attested split holds, and the rows sum to the namespace
// figure the ModelProvider status reports.
var _ = Describe("Per-workload spend (#100)", Ordered, func() {
	It("provisions the provider, agents, and callers", func() {
		// Self-contained: every fixture this spec touches is applied here
		// (kubectl apply is idempotent when other specs share them).
		for _, f := range []string{
			"test/e2e/testdata/namespace.yaml",
			"test/e2e/testdata/mockprovider.yaml",
			"test/e2e/testdata/console.yaml",
			"test/e2e/testdata/spend.yaml",
		} {
			_, err := utils.Kubectl("apply", "-f", f)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "console-spend",
				`{.status.conditions[?(@.type=="Ready")].status}`)
		}, "60s", "3s").Should(Equal("True"))
		for _, agent := range []string{"spend-a", "spend-b"} {
			Eventually(func() (string, error) {
				return utils.ResourceField("agent", "console-e2e", agent, "{.status.phase}")
			}, "180s", "5s").Should(Equal("Running"))
		}
	})

	It("the callers complete their attested LLM calls", func() {
		for _, pod := range []string{"spend-caller-a", "spend-caller-b"} {
			Eventually(func() (string, error) {
				return utils.ResourceField("pod", "console-e2e", pod, "{.status.phase}")
			}, "240s", "5s").Should(Equal("Succeeded"), pod)
		}
	})

	It("the console shows each agent's spend separately", func() {
		token, err := utils.Kubectl("create", "token", "console-viewer", "-n", "console-e2e")
		Expect(err).NotTo(HaveOccurred())
		viewerToken := strings.TrimSpace(token)

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-console", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		url := fmt.Sprintf("https://127.0.0.1:%d/api/v1/namespaces/console-e2e/spend", port)

		// bigusage returns 5M+5M tokens; at 1.00/2.00 per 1M each call costs
		// exactly 15.00 USD: one call for spend-a, two for spend-b. The
		// breakdown converges within one publish interval (10s).
		var workloads map[string]string
		Eventually(func() (bool, error) {
			status, body, err := utils.GetWithBearer(url, viewerToken)
			if err != nil || status != 200 {
				return false, err
			}
			var out struct {
				Workloads []struct {
					Provider string `json:"provider"`
					Rows     []struct {
						Workload string `json:"workload"`
						SpentUSD string `json:"spentUSD"`
					} `json:"rows"`
				} `json:"workloads"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				return false, err
			}
			workloads = map[string]string{}
			for _, ws := range out.Workloads {
				if ws.Provider != "console-spend" {
					continue
				}
				for _, row := range ws.Rows {
					workloads[row.Workload] = row.SpentUSD
				}
			}
			return workloads["agent/spend-a"] == "15.00" && workloads["agent/spend-b"] == "30.00", nil
		}, "90s", "5s").Should(BeTrue(), func() string { return fmt.Sprintf("workloads = %v", workloads) })

		// mTLS-attested callers only: nothing lands unattributed here.
		Expect(workloads).NotTo(HaveKey("(unattributed)"))
	})

	It("the rows sum to the namespace figure the provider status reports", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "console-spend",
				`{.status.budgetUsage[?(@.namespace=="console-e2e")].spentUSD}`)
		}, "180s", "5s").Should(Equal("45.00"))

		out, err := utils.ResourceField("modelprovider", "", "console-spend",
			`{.status.budgetUsage[?(@.namespace=="console-e2e")].percentUsed}`)
		Expect(err).NotTo(HaveOccurred())
		pct, err := strconv.Atoi(strings.TrimSpace(out))
		Expect(err).NotTo(HaveOccurred())
		Expect(pct).To(Equal(45))
	})
})
