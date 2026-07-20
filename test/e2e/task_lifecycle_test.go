//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

var _ = Describe("Task lifecycle", Ordered, func() {
	BeforeAll(func() {
		// Ensure the class exists even when this spec runs in isolation.
		_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
	})

	It("runs an agentReported AgentTask to Succeeded", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agenttask.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agenttask", "e2e", "e2e-task", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Succeeded"))
	})

	It("created the per-task completion mailbox ConfigMap and Role", func() {
		// agentReported tasks get a completion mailbox ConfigMap
		// (<task>-completion) and a per-task Role
		// (agentry-task-<task>-completion) granting the gateway completion
		// access, both in the task's own namespace (not agentry-system). The
		// ConfigMap carries no labels (see desiredCompletionConfigMap in
		// internal/controller/agenttask_desired.go), so it must be looked up
		// by name rather than by the agentry.io/task pod label.
		Eventually(func() error {
			_, err := utils.Kubectl("get", "configmap", "e2e-task-completion", "-n", "e2e")
			return err
		}, "30s", "3s").Should(Succeed())
		out, err := utils.Kubectl("get", "role", "-n", "e2e", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("e2e-task"))
	})

	It("garbage-collects the task Pod after ttlSecondsAfterFinished", func() {
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "agentry.io/task=e2e-task", "--no-headers")
		}, "90s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))
	})
})
