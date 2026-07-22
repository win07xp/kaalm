//go:build e2e

package e2e

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// absent reports whether a resource is gone, treating a NotFound as success and
// any other error as a real failure. utils.Kubectl folds kubectl's combined
// output into the error, so NotFound is distinguishable from a transport fault.
func absent(kind, namespace, name string) func() (bool, error) {
	return func() (bool, error) {
		out, err := utils.Kubectl("get", kind, name, "-n", namespace)
		if err == nil {
			return false, nil
		}
		if strings.Contains(out, "NotFound") || strings.Contains(err.Error(), "NotFound") {
			return true, nil
		}
		return false, err
	}
}

// S11: deleting an Agent runs the finalizer, which gracefully terminates the
// Pod and then honors the class pvcRetention policy before releasing the
// object. envtest covers the ownerRef bookkeeping; only a real cluster shows
// the finalizer actually completing, the kubelet really terminating the Pod,
// and garbage collection really removing (or really sparing) the PVC.
var _ = Describe("Clean teardown on delete (S11)", Ordered, func() {
	var retainPVCUID string

	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/teardown.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("both retention classes reconcile to Ready")
		for _, class := range []string{"s11-retain", "s11-delete"} {
			Eventually(func() (bool, error) {
				return readyTrue("agentclass", "", class)
			}, "60s", "3s").Should(BeTrue(), "class %s should be Ready", class)
		}

		By("both agents provision to Running with their PVCs")
		for _, agent := range []string{"s11-retain-agent", "s11-delete-agent"} {
			Eventually(func() (string, error) {
				return utils.ResourceField("agent", "e2e", agent, "{.status.phase}")
			}, "180s", "5s").Should(Equal("Running"), "agent %s should be Running", agent)
			Eventually(func() (string, error) {
				return utils.ResourceField("pvc", "e2e", agent+"-memory", "{.metadata.uid}")
			}, "60s", "3s").ShouldNot(BeEmpty(), "PVC for %s should exist", agent)
		}
		retainPVCUID, _ = utils.ResourceField("pvc", "e2e", "s11-retain-agent-memory", "{.metadata.uid}")
		Expect(retainPVCUID).NotTo(BeEmpty())
	})

	It("removes the Agent, its Pod, and its PVC under pvcRetention Delete", func() {
		_, err := utils.Kubectl("delete", "agent", "s11-delete-agent", "-n", "e2e", "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		By("the finalizer completes and the Agent is released")
		// A stuck finalizer leaves the object Terminating forever, so this is
		// the assertion that the drain path actually finishes.
		Eventually(absent("agent", "e2e", "s11-delete-agent"), "120s", "5s").Should(BeTrue())

		By("the Pod is gone")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "kaalm.io/agent=s11-delete-agent", "--no-headers")
		}, "60s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))

		By("cascade garbage collection removes the PVC")
		Eventually(absent("pvc", "e2e", "s11-delete-agent-memory"), "90s", "5s").Should(BeTrue())
	})

	It("keeps the PVC under pvcRetention Retain", func() {
		_, err := utils.Kubectl("delete", "agent", "s11-retain-agent", "-n", "e2e", "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		By("the finalizer completes and the Agent is released")
		Eventually(absent("agent", "e2e", "s11-retain-agent"), "120s", "5s").Should(BeTrue())

		By("the Pod is gone")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "kaalm.io/agent=s11-retain-agent", "--no-headers")
		}, "60s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))

		By("the same PVC survives, and keeps surviving")
		// Consistently, not a single read: the finalizer strips the ownerRef
		// before releasing the Agent, so a merely-slow collector must not be
		// able to pass this by deleting the PVC a moment later.
		Consistently(func() (string, error) {
			return utils.ResourceField("pvc", "e2e", "s11-retain-agent-memory", "{.metadata.uid}")
		}, "15s", "3s").Should(Equal(retainPVCUID))

		// The retained PVC outlives its Agent by design, so drop it explicitly
		// rather than leaning on the namespace teardown.
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "pvc", "s11-retain-agent-memory", "-n", "e2e",
				"--ignore-not-found", "--wait=false")
		})
	})
})
