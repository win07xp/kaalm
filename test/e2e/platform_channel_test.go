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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// platformChannel names the objects a platform channel spec (S22 Discord,
// S23 WhatsApp) brings up in namespace e2e.
type platformChannel struct {
	// Manifest is the testdata file holding the mock, Agent, Secret, and
	// AgentChannel.
	Manifest string
	// Mock is the mock platform's Deployment and Service.
	Mock string
	// Agent, Channel, and Secret are the Agent, the AgentChannel, and the
	// channel's credential Secret.
	Agent, Channel, Secret string
	// Platform names the platform in the By text.
	Platform string
}

// applyPlatformChannel applies the shared AgentClass and pc's manifest, waits
// for the mock to roll out and the Agent to run, checks the channel goes
// Ready with its credential Role scoped to the Secret, and port-forwards to
// the mock. It returns the port-forward's stop function and the mock's URL.
func applyPlatformChannel(pc platformChannel) (stop func(), mockURL string) {
	_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
	_, err := utils.Kubectl("apply", "-f", pc.Manifest)
	Expect(err).NotTo(HaveOccurred())
	Expect(utils.WaitRollout("e2e", pc.Mock, "120s")).To(Succeed())
	Eventually(func() (string, error) {
		return utils.ResourceField("agent", "e2e", pc.Agent, "{.status.phase}")
	}, "180s", "5s").Should(Equal("Running"))

	By("the " + pc.Platform + " channel reconciles to Ready with its credential Role scoped to the Secret")
	Eventually(func() (string, error) {
		return utils.ResourceField("agentchannel", "e2e", pc.Channel,
			`{.status.conditions[?(@.type=="Ready")].status}`)
	}, "90s", "3s").Should(Equal("True"))
	names, err := utils.ResourceField("role", "e2e", "kaalm-channel-"+pc.Channel+"-creds", "{.rules[0].resourceNames}")
	Expect(err).NotTo(HaveOccurred())
	Expect(names).To(ContainSubstring(pc.Secret))

	By("the controller holds an open watch on a Secret in namespace e2e, so the channel's check Role grants list")
	expectControllerWatchesSecrets()

	port, stop, err := utils.PortForward("e2e", pc.Mock, "8080")
	Expect(err).NotTo(HaveOccurred())
	return stop, fmt.Sprintf("http://127.0.0.1:%d", port)
}

// expectControllerWatchesSecrets asserts that some controller replica holds
// an open watch on a user-namespace Secret. The controller's Secret watcher
// holds only user-namespace Secrets (kaalm-system ones come from the manager
// cache), and a Role whose list is refused falls back to a get per read and
// is not counted, so a value above zero shows a minted Role's name-selected
// list passed RBAC. Only the leader reads Secrets and a standby reports 0, so
// every controller Pod is scraped.
func expectControllerWatchesSecrets() {
	out, err := utils.Kubectl("get", "pods", "-n", "kaalm-system",
		"-l", "app.kubernetes.io/component=controller", "-o", "jsonpath={.items[*].metadata.name}")
	Expect(err).NotTo(HaveOccurred())
	pods := strings.Fields(out)
	Expect(pods).NotTo(BeEmpty())

	var ports []int
	for _, pod := range pods {
		port, stop, err := utils.PortForwardTarget("kaalm-system", "pod/"+pod, "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		ports = append(ports, port)
	}
	Eventually(func() string {
		var pages []string
		for _, port := range ports {
			// A replica that fails one scrape is read again on the next poll.
			if page, err := scrape(fmt.Sprintf("http://127.0.0.1:%d/metrics", port)); err == nil {
				pages = append(pages, page)
			}
		}
		return strings.Join(pages, "\n")
	}, "60s", "3s").Should(MatchRegexp(`(?m)^kaalm_watched_secrets [1-9]`),
		"no controller replica holds an open watch on a user-namespace Secret: "+
			"the leader read the channel's Secret with get only, so the check Role's list was refused")
}
