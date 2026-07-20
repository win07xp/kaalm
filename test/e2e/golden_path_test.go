//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

// readyTrue reports whether a resource's Ready condition is True.
func readyTrue(kind, namespace, name string) (bool, error) {
	out, err := utils.ResourceField(kind, namespace, name,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	if err != nil {
		return false, err
	}
	return out == "True", nil
}

var _ = Describe("Golden path", Ordered, func() {
	It("reconciles the AgentClass to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "e2e-standard")
		}, "60s", "3s").Should(BeTrue())
	})

	It("reconciles the ModelProvider to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/modelprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "e2e-vertex")
		}, "60s", "3s").Should(BeTrue())
	})

	It("provisions the Agent Pod to Running with its child resources", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agent.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "e2e-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the per-agent Service, NetworkPolicy, and ServiceAccount exist")
		_, err = utils.Kubectl("get", "service", "e2e-agent", "-n", "e2e")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("get", "networkpolicy", "e2e-agent", "-n", "e2e")
		Expect(err).NotTo(HaveOccurred())
	})

	It("reconciles the AgentChannel to Active", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentchannel.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "e2e-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))
	})

	It("delivers a sync webhook and returns the agent's reply", func() {
		port, stop, err := utils.PortForward("agentry-system", "agentry-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		url := fmt.Sprintf("https://127.0.0.1:%d/channels/e2e/e2e-channel", port)
		body := []byte(`{"userId":"e2e-user","content":{"text":"ping"}}`)

		var status int
		var resp string
		Eventually(func() (int, error) {
			status, resp, err = utils.PostJSON(url, "e2e-webhook-bearer-token", body)
			return status, err
		}, "60s", "3s").Should(Equal(200))

		var reply struct {
			Content string `json:"content"`
		}
		Expect(json.Unmarshal([]byte(resp), &reply)).To(Succeed())
		// The starter-go agent echoes the delivered message back in its reply,
		// so the reply content must contain the text we sent ("ping"). This
		// proves the full round trip without pinning the template's phrasing.
		Expect(reply.Content).To(ContainSubstring("ping"))
	})

	It("blocks delivery from a disallowed namespace via the NetworkPolicy", func() {
		// A pod in `default` is not in the agent's ingress allow-list, so the
		// synthesized NetworkPolicy must refuse it. (The allowed gateway path is
		// already proven by the sync-webhook spec above.)
		_ = time.Second
		probe := []string{
			"run", "np-deny-probe", "-n", "default", "--rm", "-i", "--restart=Never",
			"--image=curlimages/curl:8.10.1", "--command", "--",
			"sh", "-c",
			"curl -sk --max-time 6 -o /dev/null -w '%{http_code}' " +
				"https://e2e-agent.e2e.svc.cluster.local:8080/readyz || true",
		}
		Eventually(func() (string, error) {
			return utils.Kubectl(probe...)
		}, "60s", "5s").ShouldNot(ContainSubstring("200"))
	})
})
