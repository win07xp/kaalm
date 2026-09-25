//go:build e2e

package e2e

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// FQDN egress: allowedHosts becomes a CiliumNetworkPolicy toFQDNs rule, and
// the rule lets the workload reach the named host and nothing else. It runs
// only where the CNI is Cilium; k3d's default CNI has no cilium.io group.
var _ = Describe("FQDN egress on a Cilium CNI", Ordered, func() {
	BeforeAll(func() {
		out, err := utils.Kubectl("api-resources", "--api-group=cilium.io", "-o", "name")
		if err != nil || !strings.Contains(out, "ciliumnetworkpolicies") {
			Skip("CNI has no cilium.io API group")
		}
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/fqdn.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/fqdn-caller.yaml", "--ignore-not-found")
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/fqdn.yaml", "--ignore-not-found")
		})
	})

	It("synthesizes the CiliumNetworkPolicy for the class's allowedHosts", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "fqdn-agent", "{.status.phase}")
		}, "60s", "3s").Should(Equal("Running"))
		Eventually(func() (string, error) {
			return utils.ResourceField("ciliumnetworkpolicies.cilium.io", "e2e", "fqdn-agent-fqdn",
				"{.spec.egress[*].toFQDNs[*].matchName}")
		}, "10s", "2s").Should(Equal("example.com"))
		owner, err := utils.ResourceField("ciliumnetworkpolicies.cilium.io", "e2e", "fqdn-agent-fqdn",
			"{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(owner).To(Equal("Agent/fqdn-agent"))
	})

	It("reaches the allowed host and not another", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/fqdn-caller.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "fqdn-caller", "{.status.phase}")
		}, "45s", "3s").Should(Equal("Succeeded"))
		logs, err := utils.Kubectl("logs", "-n", "e2e", "fqdn-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("example.com: ok"))
		Expect(logs).To(ContainSubstring("example.org: blocked"))
	})
})
