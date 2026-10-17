//go:build e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

var kaalmCRDs = []string{
	"agentclasses.kaalm.io", "modelproviders.kaalm.io", "toolproviders.kaalm.io",
	"agents.kaalm.io", "agenttasks.kaalm.io", "agentchannels.kaalm.io",
}

// The API on a fresh install (docs/src/operations/api-versioning.md):
// v1beta1 is the one served version and the storage version, and the
// controller's storage-version migrator runs on the leader. S21 (the
// upgrade e2e) proves the upgrade story.
var _ = Describe("API versions (v1beta1 stored and migrated)", func() {
	It("serves and stores only v1beta1, with no conversion webhook", func() {
		for _, crd := range kaalmCRDs {
			out, err := utils.Kubectl("get", "crd", crd, "-o",
				"jsonpath={.spec.versions[*].name}|{.status.storedVersions}|{.spec.conversion.strategy}")
			Expect(err).NotTo(HaveOccurred(), crd)
			Expect(out).To(Equal(`v1beta1|["v1beta1"]|None`), crd)
		}
		_, err := utils.Kubectl("get", "agentclasses.v1alpha1.kaalm.io")
		Expect(err).To(HaveOccurred(), "kaalm.io/v1alpha1 is no longer served")
	})

	It("ran the storage-version migrator on the leader, with the RBAC the chart grants", func() {
		// A fresh install has nothing to move, so the migrator's in-cluster
		// proof here is that the pass ran and completed under the chart's
		// ClusterRole (the get on CRDs by name is the grant that would fail
		// first if it were missing). Its envtest proves the moving itself.
		var logs string
		Eventually(func() string {
			out, _ := utils.Kubectl("logs", "-n", "kaalm-system", "-l", "app.kubernetes.io/component=controller",
				"--tail=-1", "--prefix")
			logs = out
			return out
		}, 2*time.Minute, 5*time.Second).Should(ContainSubstring("storage-version migration pass complete"))
		Expect(logs).NotTo(ContainSubstring("storage-version migration failed"))
		Expect(logs).To(MatchRegexp(`"kindsAlreadyCurrent":\s*6`))
	})
})
