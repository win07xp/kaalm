//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// S9 promotes a finished task's PVC to a persistent Agent. On k3d the snapshot
// and clone are standard Kubernetes and stay documented, not CI-proven (the
// local-path provisioner has no VolumeSnapshot support). This spec proves the
// Kaalm-owned primitive underneath the promotion: a persistent Agent adopts a
// pre-populated PVC via spec.persistence.existingClaim (the controller mounts
// that claim and provisions none of its own), and the promoted state is read
// back through the adopted volume. A writer pod stands in for the task's write.
var _ = Describe("Promotion via existingClaim", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/promotion.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "s9-persistent")
		}, "60s", "3s").Should(BeTrue())
	})

	It("seeds the adopted claim with state via a writer", func() {
		// The writer is the claim's first consumer, so applying it binds the
		// WaitForFirstConsumer PVC; the data then persists on the node for the
		// Agent and reader that mount it afterward.
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/promotion-writer.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s9-writer", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Succeeded"))
	})

	It("provisions a persistent Agent that adopts the existing claim", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/promotion-agent.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s9-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the Agent Pod mounts the adopted claim by name")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e", "-l", "kaalm.io/agent=s9-agent",
				"-o", `jsonpath={.items[0].spec.volumes[?(@.name=="agent-memory")].persistentVolumeClaim.claimName}`)
		}, "30s", "3s").Should(Equal("s9-state"))

		By("the controller provisioned no per-agent PVC of its own")
		out, err := utils.Kubectl("get", "pvc", "s9-agent-memory", "-n", "e2e")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("NotFound"))
	})

	It("reads the promoted state back through the adopted claim", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/promotion-reader.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "s9-reader", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Succeeded"))

		out, err := utils.Kubectl("logs", "-n", "e2e", "s9-reader")
		Expect(err).NotTo(HaveOccurred())
		// The marker the writer stored, read back from the claim the new Agent
		// adopted: the promoted state survived and is present in the Agent's
		// volume.
		Expect(out).To(ContainSubstring("PROMOTED-s9"))
	})
})
