//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// agentPodField reads a jsonpath field off the first Pod owned by an Agent
// (Pods carry the kaalm.io/agent label). Returns "" when no Pod exists yet.
func agentPodField(name, jsonpath string) (string, error) {
	return utils.Kubectl("get", "pods", "-n", "e2e",
		"-l", "kaalm.io/agent="+name, "-o", "jsonpath="+jsonpath)
}

// S2: the sandboxed AgentClass. k3d has no gVisor, so this proves the
// Kaalm-owned parts only: runtimeClassName passthrough and the image allowlist.
var _ = Describe("Sandboxed class (S2)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/sandboxed-class.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/sandboxed-class.yaml", "--ignore-not-found")
		})
	})

	It("passes the class runtimeClassName through to the agent Pod", func() {
		Eventually(func() (string, error) {
			return agentPodField("s2-agent", "{.items[0].spec.runtimeClassName}")
		}, "180s", "5s").Should(Equal("gvisor"))
	})

	It("rejects an agent whose image is outside the class allowlist", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s2-bad-agent", "{.status.phase}")
		}, "120s", "5s").Should(Equal("Degraded"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s2-bad-agent",
				`{.status.conditions[?(@.type=="Ready")].reason}`)
		}, "120s", "5s").Should(Equal("ClassConstraintViolation"))
	})
})

// S5: revoking a namespace's provider access denies its next call and degrades
// its agents, without evicting the running Pods.
var _ = Describe("Access revocation (S5)", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/s5-access.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s5-access.yaml", "--ignore-not-found")
		})
		By("the agent reaches Ready while access is granted")
		Eventually(func() (bool, error) {
			return readyTrue("agent", "e2e", "s5-agent")
		}, "180s", "5s").Should(BeTrue())
	})

	It("denies access, degrades the agent, and leaves its Pod running", func() {
		By("the agent Pod is Running before revocation")
		Eventually(func() (string, error) {
			return agentPodField("s5-agent", "{.items[0].status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("revoking: drop e2e from the provider's allowedNamespaces")
		_, err := utils.Kubectl("patch", "modelprovider", "s5-provider", "--type=json",
			"-p", `[{"op":"replace","path":"/spec/allowedNamespaces","value":["s5-revoked"]}]`)
		Expect(err).NotTo(HaveOccurred())

		By("(a) the next token-authenticated call is denied 403 access_denied")
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/s5-caller.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/s5-caller.yaml", "--ignore-not-found")
		})
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s5-caller", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Succeeded"))
		logs, err := utils.Kubectl("logs", "-n", "e2e", "s5-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("HTTP 403"))
		Expect(logs).To(ContainSubstring("access_denied"))

		By("(b) the agent transitions to Degraded / ClassConstraintViolation")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s5-agent", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Degraded"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s5-agent",
				`{.status.conditions[?(@.type=="Ready")].reason}`)
		}, "120s", "3s").Should(Equal("ClassConstraintViolation"))

		By("(c) the agent Pod keeps Running: revocation is not eviction")
		phase, err := agentPodField("s5-agent", "{.items[0].status.phase}")
		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal("Running"))
	})
})
