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

// syncReply POSTs text to the named sync channel and returns the reply
// content. Non-200 statuses come back as errors so callers can retry inside
// an Eventually while the target pod is still starting or being replaced.
func syncReply(port int, channel, bearer, text string) (string, error) {
	url := fmt.Sprintf("https://127.0.0.1:%d/channels/e2e/%s", port, channel)
	body := []byte(fmt.Sprintf(`{"userId":"s16-user","content":{"text":%q}}`, text))
	status, resp, err := utils.PostJSON(url, bearer, body)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("status %d: %s", status, resp)
	}
	var reply struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(resp), &reply); err != nil {
		return "", fmt.Errorf("unmarshal %q: %w", resp, err)
	}
	return reply.Content, nil
}

var _ = Describe("Zero-build on-ramp (S16)", Ordered, func() {
	It("reconciles the starter class that allowlists only the base images", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/base-image.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "s16-starter")
		}, "60s", "3s").Should(BeTrue())
	})

	It("answers through the Go base image's built-in default handler", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s16-go", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s16-go-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		// Exact equality pins the default handler's reply shape: this is the
		// output the mounted-handler assertions below must be distinguishable
		// from.
		Eventually(func() (string, error) {
			return syncReply(port, "s16-go-channel", "s16-webhook-bearer-token", "ping")
		}, "60s", "3s").Should(Equal("echo: ping"))
	})

	It("answers from the mounted Python handler, not the default", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s16-py", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s16-py-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		// "greeter v1" can only come from the ConfigMap-mounted handler; the
		// image's built-in default would say "echo: ping".
		Eventually(func() (string, error) {
			return syncReply(port, "s16-py-channel", "s16-webhook-bearer-token", "ping")
		}, "60s", "3s").Should(Equal("greeter v1: ping"))
	})

	It("rolls the handler by repointing the ConfigMap reference", func() {
		_, err := utils.Kubectl("patch", "agent", "s16-py", "-n", "e2e", "--type=merge",
			"-p", `{"spec":{"handler":{"configMapRef":{"name":"s16-handler-v2"}}}}`)
		Expect(err).NotTo(HaveOccurred())

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		// Repointing changes the desired-pod hash, so the Pod is replaced and
		// the reply flips once the v2 pod serves; 180s covers the replacement.
		Eventually(func() (string, error) {
			return syncReply(port, "s16-py-channel", "s16-webhook-bearer-token", "ping")
		}, "180s", "5s").Should(Equal("greeter v2: ping"))
	})

	It("gates a missing handler ConfigMap and recovers when it appears", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s16-missing",
				`{.status.conditions[?(@.type=="Ready")].reason}`)
		}, "120s", "3s").Should(Equal("HandlerConfigMapNotFound"))

		By("rule 31 gates at reconcile time: no Pod exists, as opposed to one wedged on a missing volume source")
		out, err := utils.Kubectl("get", "pods", "-n", "e2e",
			"-l", "kaalm.io/agent=s16-missing", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(BeEmpty())

		By("creating the ConfigMap recovers the gate without touching the Agent")
		_, err = utils.Kubectl("create", "configmap", "s16-missing-handler", "-n", "e2e",
			"--from-literal=handler.py="+
				"def handle_message(envelope):\n"+
				`    return {"content": "recovered", "attachments": [], "metadata": {}}`+"\n")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s16-missing", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))
	})
})
