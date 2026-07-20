//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting agentry e2e suite\n")
	RunSpecs(t, "agentry e2e suite")
}

var _ = BeforeSuite(func() {
	By("verifying the controller and gateway rollouts are Ready")
	Expect(utils.WaitRollout("agentry-system", "agentry-controller", "150s")).To(Succeed())
	Expect(utils.WaitRollout("agentry-system", "agentry-gateway", "150s")).To(Succeed())

	By("seeding the e2e namespace and secrets")
	_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/namespace.yaml")
	Expect(err).NotTo(HaveOccurred())
	_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/secrets.yaml")
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	By("tearing down e2e objects (chart and cluster are left in place)")
	// Order: workloads first, then cluster-scoped, then the namespace.
	for _, f := range []string{
		"test/e2e/testdata/agenttask.yaml",
		"test/e2e/testdata/agentchannel.yaml",
		"test/e2e/testdata/agent.yaml",
		"test/e2e/testdata/modelprovider.yaml",
		"test/e2e/testdata/agentclass.yaml",
		"test/e2e/testdata/secrets.yaml",
		"test/e2e/testdata/namespace.yaml",
	} {
		_, _ = utils.Kubectl("delete", "-f", f, "--ignore-not-found", "--wait=false")
	}
})

var _ = Describe("Deployment", func() {
	It("has all five Agentry CRDs installed", func() {
		out, err := utils.Kubectl("get", "crds", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		for _, crd := range []string{
			"agentclasses.agentry.io", "modelproviders.agentry.io", "agents.agentry.io",
			"agenttasks.agentry.io", "agentchannels.agentry.io",
		} {
			Expect(out).To(ContainSubstring(crd))
		}
	})
})
