//go:build e2e

package e2e

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// The graduated API (docs/src/operations/api-versioning.md): v1beta1 is the
// storage version, v1alpha1 is served, deprecated, and converted by the
// controller's conversion webhook, whose caBundle cert-manager's cainjector
// keeps in the CRDs. The full upgrade story is S21 (the upgrade e2e); this
// spec proves the serving side on a fresh install.
var _ = Describe("API versions (v1beta1 stored, v1alpha1 converted)", Ordered, func() {
	AfterAll(func() {
		_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/api-versions.yaml", "--ignore-not-found")
	})

	It("serves both versions, stores v1beta1, and points conversion at the controller with an injected CA", func() {
		for _, crd := range []string{
			"agentclasses.kaalm.io", "modelproviders.kaalm.io", "toolproviders.kaalm.io",
			"agents.kaalm.io", "agenttasks.kaalm.io", "agentchannels.kaalm.io",
		} {
			out, err := utils.Kubectl("get", "crd", crd, "-o",
				"jsonpath={.spec.versions[*].name}|{.status.storedVersions}|{.spec.conversion.strategy}|"+
					"{.spec.conversion.webhook.clientConfig.service.name}:{.spec.conversion.webhook.clientConfig.service.port}")
			Expect(err).NotTo(HaveOccurred(), crd)
			parts := strings.Split(out, "|")
			Expect(parts).To(HaveLen(4), out)
			Expect(strings.Fields(parts[0])).To(ConsistOf("v1alpha1", "v1beta1"), crd)
			Expect(parts[1]).To(Equal(`["v1beta1"]`), crd)
			Expect(parts[2]).To(Equal("Webhook"), crd)
			Expect(parts[3]).To(Equal("kaalm-controller:9444"), crd)

			// cainjector fills the caBundle from kaalm-controller-tls; an empty
			// bundle would mean every v1alpha1 request fails TLS verification.
			Eventually(func() (string, error) {
				return utils.Kubectl("get", "crd", crd, "-o", "jsonpath={.spec.conversion.webhook.clientConfig.caBundle}")
			}, "60s", "2s").ShouldNot(BeEmpty(), crd)
		}
	})

	It("applies a v1alpha1 manifest with the deprecation warning and reads it back at both versions", func() {
		out, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/api-versions.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("kaalm.io/v1alpha1 AgentClass is deprecated; use kaalm.io/v1beta1"))

		// Each read at an explicit version converts from the stored v1beta1
		// object (or serves it directly); both must show the applied spec. A
		// read at v1alpha1 carries the deprecation warning (kubectl prints it
		// on stderr, which the helper folds into the output); a read at
		// v1beta1 must not.
		for _, resource := range []string{"agentclasses.v1alpha1.kaalm.io", "agentclasses.v1beta1.kaalm.io"} {
			out, err := utils.Kubectl("get", resource, "api-versions-class", "-o",
				"jsonpath={.apiVersion} {.spec.image.defaultImage} {.spec.lifecycle.defaultIdleTimeout}")
			Expect(err).NotTo(HaveOccurred(), resource)
			version := strings.TrimPrefix(strings.TrimSuffix(resource, ".kaalm.io"), "agentclasses.")
			Expect(lastLine(out)).To(Equal("kaalm.io/" + version + " example/agent:api-versions 45s"))
			if version == "v1alpha1" {
				Expect(out).To(ContainSubstring("is deprecated; use kaalm.io/v1beta1"), resource)
			} else {
				Expect(out).NotTo(ContainSubstring("deprecated"), resource)
			}
		}

		// The bare kind resolves to the preferred version, v1beta1.
		out, err = utils.Kubectl("get", "agentclass", "api-versions-class", "-o", "jsonpath={.apiVersion}")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("kaalm.io/v1beta1"))

		// A write at v1beta1 is visible at v1alpha1: the spoke reads through
		// the conversion webhook every time.
		_, err = utils.Kubectl("patch", "agentclasses.v1beta1.kaalm.io", "api-versions-class", "--type=merge",
			"-p", `{"spec":{"lifecycle":{"defaultIdleTimeout":"50s"}}}`)
		Expect(err).NotTo(HaveOccurred())
		out, err = utils.Kubectl("get", "agentclasses.v1alpha1.kaalm.io", "api-versions-class", "-o",
			"jsonpath={.spec.lifecycle.defaultIdleTimeout}")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastLine(out)).To(Equal("50s"))
	})
})

// lastLine returns the final non-empty line of a kubectl output, which is the
// jsonpath result once any apiserver warnings printed before it are skipped.
func lastLine(out string) string {
	lines := utils.GetNonEmptyLines(strings.TrimSpace(out))
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
