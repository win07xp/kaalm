//go:build e2e

package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// consoleClient is a cookie-jar client for the console's server-rendered
// pages (the session cookie is Secure and the console serves TLS with the
// in-cluster CA, so verification is skipped on the localhost port-forward).
func consoleClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // localhost port-forward only
		},
	}
}

// S19: the operator console. Enable it, log in with a namespaced token, see
// the fleet with a hibernated agent shown as such, test-chat it awake, and
// watch an unauthorized token see nothing
// (docs/src/appendix/scenarios.md, S19; docs/src/console/overview.md).
var _ = Describe("Operator console (S19)", Ordered, func() {
	var viewerToken string

	It("renders no console objects on a default install", func() {
		// The suite's release runs with console.enabled=true; the default-off
		// half is proven by rendering the chart without the flag.
		out, err := utils.Helm("template", "kaalm", "charts/kaalm")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("kaalm-console"))
	})

	It("runs the console Deployment in the enabled install", func() {
		Expect(utils.WaitRollout("kaalm-system", "kaalm-console", "120s")).To(Succeed())
	})

	It("provisions the fixture agent and lets it hibernate", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/console.yaml")
		Expect(err).NotTo(HaveOccurred())
		// The fixture idles after 2s and hibernates 2s later, so a 5s poll
		// can miss the Running window entirely; asserting Running first made
		// this spec flake. Hibernated is reachable only through Running, so
		// the terminal phase alone proves both provisioning and hibernation.
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "console-e2e", "console-agent", "{.status.phase}")
		}, "300s", "3s").Should(Equal("Hibernated"))

		token, err := utils.Kubectl("create", "token", "console-viewer", "-n", "console-e2e")
		Expect(err).NotTo(HaveOccurred())
		viewerToken = strings.TrimSpace(token)
		Expect(viewerToken).NotTo(BeEmpty())
	})

	It("serves live fleet data to an authorized token, namespace-filtered", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-console", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		base := fmt.Sprintf("https://127.0.0.1:%d", port)

		By("the namespace list carries exactly what the token may read")
		var nsResp string
		Eventually(func() (int, error) {
			var status int
			status, nsResp, err = utils.GetWithBearer(base+"/api/v1/namespaces", viewerToken)
			return status, err
		}, "60s", "3s").Should(Equal(200))
		var nsList struct {
			Namespaces []string `json:"namespaces"`
		}
		Expect(json.Unmarshal([]byte(nsResp), &nsList)).To(Succeed())
		Expect(nsList.Namespaces).To(Equal([]string{"console-e2e"}))

		By("the fleet rows carry the live phase")
		status, fleet, err := utils.GetWithBearer(base+"/api/v1/namespaces/console-e2e/agents", viewerToken)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(fleet).To(ContainSubstring(`"name":"console-agent"`))
		Expect(fleet).To(ContainSubstring(`"phase":"Hibernated"`))
		Expect(fleet).To(ContainSubstring(`"hibernatedAt"`))
	})

	It("renders the namespace page through a paste-token login session", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-console", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		base := fmt.Sprintf("https://127.0.0.1:%d", port)
		c := consoleClient()

		resp, err := c.PostForm(base+"/login", url.Values{"token": {viewerToken}})
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()

		pageResp, err := c.Get(base + "/ns/console-e2e")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = pageResp.Body.Close() }()
		Expect(pageResp.StatusCode).To(Equal(200))
		page, err := io.ReadAll(pageResp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(page)).To(ContainSubstring("console-agent"))
		Expect(string(page)).To(ContainSubstring("Hibernated"))
	})

	It("test-chats the hibernated agent awake and returns the reply", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-console", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		chatURL := fmt.Sprintf(
			"https://127.0.0.1:%d/api/v1/namespaces/console-e2e/agents/console-agent/chat", port)
		body := []byte(`{"content":"ping from the console"}`)

		// The first calls trigger the wake and can outrun the 30s sync
		// deadline while the fresh pod waits out the ipset lag; retry until
		// the reply lands.
		var resp string
		Eventually(func() (int, error) {
			var status int
			var err error
			status, resp, err = utils.PostJSON(chatURL, viewerToken, body)
			return status, err
		}, "240s", "5s").Should(Equal(200))

		var reply struct {
			Content string `json:"content"`
		}
		Expect(json.Unmarshal([]byte(resp), &reply)).To(Succeed())
		Expect(reply.Content).To(ContainSubstring("ping from the console"))

		By("the wake is visible on the resource")
		// The fixture idles 2s after its last activity and the controller reads
		// gateway activity through a 15s cache, so after the reply the phase is
		// Running only briefly and not at a predictable instant (a 3s poll has
		// missed it). The Woken event is the deterministic record that the
		// test-chat woke the agent; the reply above already proves delivery.
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "events", "-n", "console-e2e",
				"--field-selector", "involvedObject.name=console-agent,reason=Woken",
				"-o", "jsonpath={.items[*].reason}")
		}, "60s", "3s").Should(ContainSubstring("Woken"))
	})

	It("shows nothing to an unauthorized token", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-console", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		base := fmt.Sprintf("https://127.0.0.1:%d", port)

		outsider, err := utils.Kubectl("create", "token", "default", "-n", "default")
		Expect(err).NotTo(HaveOccurred())

		status, body, err := utils.GetWithBearer(base+"/api/v1/namespaces", strings.TrimSpace(outsider))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(strings.TrimSpace(body)).To(Equal(`{"namespaces":[]}`))

		status, _, err = utils.GetWithBearer(base+"/api/v1/namespaces/console-e2e/agents", strings.TrimSpace(outsider))
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(403))

		status, _, err = utils.GetWithBearer(base+"/api/v1/namespaces", "garbage")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(401))
	})
})
