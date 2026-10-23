//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// An Agent and an AgentTask may share a name in a namespace: the task's
// NetworkPolicy, Certificate, and TLS Secret carry a .task qualifier, so
// neither workload finds its child name taken by the other's.
var _ = Describe("Agent and AgentTask with the same name", Ordered, func() {
	const fixture = "test/e2e/testdata/shared-name.yaml"

	BeforeAll(func() {
		// Top-level containers run in random order, so this one applies the
		// shared class itself and never deletes it.
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("apply", "-f", fixture)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = utils.Kubectl("delete", "-f", fixture, "--ignore-not-found", "--timeout=90s")
	})

	owner := func(kind, name string) (string, error) {
		return utils.ResourceField(kind, "e2e", name,
			"{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}")
	}

	It("runs both workloads", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agenttask", "e2e", "shared-name", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Succeeded"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "shared-name", "{.status.phase}")
		}, "120s", "3s").Should(Equal("Running"))
	})

	It("gives each workload its own NetworkPolicy and Certificate", func() {
		for _, c := range []struct{ kind, name, want string }{
			{"networkpolicy", "shared-name", "Agent/shared-name"},
			{"networkpolicy", "shared-name.task", "AgentTask/shared-name"},
			{"certificates.cert-manager.io", "shared-name-tls", "Agent/shared-name"},
			{"certificates.cert-manager.io", "shared-name.task-tls", "AgentTask/shared-name"},
		} {
			Expect(owner(c.kind, c.name)).To(Equal(c.want), "%s %s", c.kind, c.name)
		}
	})

	It("gives each workload its own TLS Secret", func() {
		secret := func(cert string) string {
			v, err := utils.ResourceField("certificates.cert-manager.io", "e2e", cert, "{.spec.secretName}")
			Expect(err).NotTo(HaveOccurred())
			return v
		}
		agentSecret, taskSecret := secret("shared-name-tls"), secret("shared-name.task-tls")
		Expect(agentSecret).To(HavePrefix("shared-name-tls-"))
		Expect(taskSecret).To(HavePrefix("shared-name.task-tls-"))
		Expect(taskSecret).NotTo(Equal(agentSecret))
	})
})
