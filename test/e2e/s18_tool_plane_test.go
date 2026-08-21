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

// markerLine returns the first log line carrying "MARKER <step>", so
// assertions bind to one step's output instead of the whole log.
func markerLine(logs, step string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "MARKER "+step) {
			return line
		}
	}
	return ""
}

// S18: the tool plane proven on a real cluster. A mock MCP server that
// REQUIRES the kaalm-system credential sits behind a ToolProvider; a caller
// carrying the agent's mTLS identity walks the chapter's beats through the
// broker; a bearer caller outside allowedNamespaces is refused. The mock's
// request counters prove that denied calls never reached the upstream (the
// S17 counter pattern), and the gateway logs carry the audit record.
var _ = Describe("Governed tool access (S18)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/s18-toolplane.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s18-caller.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s18-outsider.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s18-toolplane.yaml", "--ignore-not-found", "--wait=false")
		})
	})

	It("reconciles the ToolProvider to Ready with its credential in kaalm-system", func() {
		Eventually(func() (bool, error) {
			return readyTrue("toolprovider", "", "search-tools")
		}, "60s", "3s").Should(BeTrue())
	})

	It("runs the granted agent and the mock MCP server", func() {
		Expect(utils.WaitRollout("e2e", "mock-mcp", "120s")).To(Succeed())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s18-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the controller's own MCP probe (initialize, tools/list) succeeds on-cluster (#86)")
		Eventually(func() (string, error) {
			return utils.ResourceField("toolprovider", "", "search-tools",
				`{.status.conditions[?(@.type=="Healthy")].status}`)
		}, "120s", "5s").Should(Equal("True"))
	})

	It("holds no tool credential anywhere in the workload namespace", func() {
		By("the credential Secret exists only in kaalm-system")
		_, err := utils.Kubectl("get", "secret", "e2e-tool-key", "-n", "kaalm-system")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("get", "secret", "e2e-tool-key", "-n", "e2e")
		Expect(err).To(HaveOccurred(), "the tool credential Secret must not exist in the workload namespace")

		By("the agent Pod references neither the Secret nor its value")
		podJSON, err := agentPodField("s18-agent", "{.items[0].spec}")
		Expect(err).NotTo(HaveOccurred())
		Expect(podJSON).NotTo(BeEmpty())
		Expect(podJSON).NotTo(ContainSubstring("e2e-tool-key"))
		Expect(podJSON).NotTo(ContainSubstring("e2e-tool-credential"))
	})

	It("lists filtered tools, calls through the broker, and is denied per grant", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/s18-caller.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s18-caller", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "e2e", "s18-caller")
		Expect(err).NotTo(HaveOccurred())

		By("initialize succeeded and the session id came back wrapped, not raw")
		Expect(markerLine(logs, "initialize")).To(ContainSubstring("HTTP 200 session-wrapped"), logs)

		By("tools/list is filtered to the granted set")
		list := markerLine(logs, "tools-list")
		Expect(list).To(ContainSubstring("HTTP 200"), logs)
		Expect(list).To(ContainSubstring("web_search"))
		Expect(list).NotTo(ContainSubstring("fetch_page"),
			"the model must never see a tool the workload cannot call")

		By("a granted tool call round-trips")
		granted := markerLine(logs, "call-granted")
		Expect(granted).To(ContainSubstring("HTTP 200"), logs)
		Expect(granted).To(ContainSubstring("called web_search"))

		By("an ungranted (cataloged) tool gets the distinct tool_denied")
		ungranted := markerLine(logs, "call-ungranted")
		Expect(ungranted).To(ContainSubstring("HTTP 403"), logs)
		Expect(ungranted).To(ContainSubstring("tool_denied"))

		By("a foreign session id is rejected as not owned by the caller")
		forged := markerLine(logs, "forged-session")
		Expect(forged).To(ContainSubstring("HTTP 403"), logs)
		Expect(forged).To(ContainSubstring("access_denied"))
	})

	It("denies a namespace outside allowedNamespaces", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/s18-outsider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "default", "s18-outsider", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Succeeded"))

		logs, err := utils.Kubectl("logs", "-n", "default", "s18-outsider")
		Expect(err).NotTo(HaveOccurred())
		outsider := markerLine(logs, "outsider")
		Expect(outsider).To(ContainSubstring("HTTP 403"), logs)
		Expect(outsider).To(ContainSubstring("access_denied"))
	})

	It("kept denied calls off the upstream and audited every brokered one", func() {
		By("the mock's counters saw only what the broker admitted")
		port, stop, err := utils.PortForward("e2e", "mock-mcp", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		status, body, err := utils.GetWithBearer(
			fmt.Sprintf("https://127.0.0.1:%d/introspect/requests", port), "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))

		var intro struct {
			Methods map[string]int `json:"methods"`
			Tools   map[string]int `json:"tools"`
		}
		Expect(json.Unmarshal([]byte(body), &intro)).To(Succeed())
		Expect(intro.Tools["web_search"]).To(Equal(1), "exactly the one granted call reached the server")
		Expect(intro.Tools).NotTo(HaveKey("fetch_page"),
			"the tool_denied call must never reach the upstream")
		// The S18 boundary is the tools surface: exactly one granted call
		// forwarded, the denied and forged-session ones refused pre-forward
		// (their 403s are asserted in the earlier specs). The initialize
		// count stopped being exact when the controller's liveness probe
		// (#86) began running its own initialize plus tools/list against
		// this mock on a 15s cadence.
		Expect(intro.Methods["initialize"]).To(BeNumerically(">=", 1))
		Expect(intro.Methods["tools/call"]).To(Equal(1))

		By("the gateway logs carry the audit record for the brokered calls")
		gwLogs, err := utils.Kubectl("logs", "-n", "kaalm-system",
			"-l", "app.kubernetes.io/component=gateway", "--tail=-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(gwLogs).To(ContainSubstring(`"msg":"mcp call"`))
		Expect(gwLogs).To(ContainSubstring(`"provider":"search-tools"`))
		Expect(gwLogs).To(ContainSubstring(`"tool":"web_search"`))
		Expect(gwLogs).To(ContainSubstring(`"error_type":"tool_denied"`))
	})
})
