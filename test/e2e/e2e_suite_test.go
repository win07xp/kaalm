//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting kaalm e2e suite\n")
	RunSpecs(t, "kaalm e2e suite")
}

var _ = BeforeSuite(func() {
	By("verifying the controller and gateway rollouts are Ready")
	Expect(utils.WaitRollout("kaalm-system", "kaalm-controller", "150s")).To(Succeed())
	Expect(utils.WaitRollout("kaalm-system", "kaalm-gateway", "150s")).To(Succeed())

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
		"test/e2e/testdata/teardown.yaml",
		"test/e2e/testdata/session_callback.yaml",
		"test/e2e/testdata/hibernation.yaml",
		"test/e2e/testdata/promotion-reader.yaml",
		"test/e2e/testdata/promotion-writer.yaml",
		"test/e2e/testdata/promotion-agent.yaml",
		"test/e2e/testdata/promotion.yaml",
		"test/e2e/testdata/agentchannel.yaml",
		"test/e2e/testdata/agent.yaml",
		"test/e2e/testdata/modelprovider.yaml",
		"test/e2e/testdata/agentclass.yaml",
		"test/e2e/testdata/secrets.yaml",
		"test/e2e/testdata/namespace.yaml",
	} {
		_, _ = utils.Kubectl("delete", "-f", f, "--ignore-not-found", "--wait=false")
	}
	// Block until the namespace is fully gone so a rapid re-run's BeforeSuite
	// does not re-apply into a still-Terminating namespace (which fails, and
	// --ignore-not-found does not help a terminating-not-absent namespace).
	_, _ = utils.Kubectl("wait", "--for=delete", "namespace/e2e", "--timeout=120s")
})

var _ = Describe("Deployment", func() {
	It("has all five Kaalm CRDs installed", func() {
		out, err := utils.Kubectl("get", "crds", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		for _, crd := range []string{
			"agentclasses.kaalm.io", "modelproviders.kaalm.io", "agents.kaalm.io",
			"agenttasks.kaalm.io", "agentchannels.kaalm.io",
		} {
			Expect(out).To(ContainSubstring(crd))
		}
	})
})
