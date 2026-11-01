//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// vertexKeySecret writes the e2e-vertex-mock-key Secret manifest: a
// service-account JSON key, generated now, whose token_uri is the mock's
// token endpoint.
func vertexKeySecret() string {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	Expect(err).NotTo(HaveOccurred())
	key, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "e2e-project", "private_key_id": "e2e",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "e2e@e2e-project.iam.gserviceaccount.com",
		"token_uri":    "https://mock-provider.e2e.svc:8443/token",
	})
	Expect(err).NotTo(HaveOccurred())
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": "e2e-vertex-mock-key", "namespace": "kaalm-system",
			"labels":      map[string]string{"kaalm.io/provider-credential": "true"},
			"annotations": map[string]string{"kaalm.io/provider-hosts": "mock-provider.e2e.svc"},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"key": string(key)},
	})
	Expect(err).NotTo(HaveOccurred())
	path := filepath.Join(GinkgoT().TempDir(), "vertex-key.json")
	Expect(os.WriteFile(path, manifest, 0o600)).To(Succeed())
	return path
}

// sumMetric adds the values of every sample of a metric family on a
// Prometheus page whose labels include all of the given name="value" pairs.
func sumMetric(page, family string, labels ...string) float64 {
	var sum float64
	for _, line := range strings.Split(page, "\n") {
		if !strings.HasPrefix(line, family+"{") {
			continue
		}
		matched := true
		for _, l := range labels {
			if !strings.Contains(line, l) {
				matched = false
				break
			}
		}
		fields := strings.Fields(line)
		if !matched || len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			sum += v
		}
	}
	return sum
}

// google-vertex served end to end: the gateway mints an access token from a
// service-account key at the mock's token endpoint, forwards both Gemini
// calls to the mock's Vertex stand-in, and meters them.
var _ = Describe("google-vertex through the gateway", Ordered, func() {
	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		secret := vertexKeySecret()
		_, err = utils.Kubectl("apply", "-f", secret)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/vertex.yaml")
		Expect(err).NotTo(HaveOccurred())
		// The provider is cluster-scoped and the Secret lives in
		// kaalm-system; the e2e namespace teardown would miss both.
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/vertex.yaml", "--ignore-not-found", "--wait=false")
			_, _ = utils.Kubectl("delete", "secret", "-n", "kaalm-system", "e2e-vertex-mock-key", "--ignore-not-found")
		})
	})

	It("reconciles the provider to Ready and Healthy through a minted token", func() {
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "e2e-vertex-mock")
		}, "60s", "3s").Should(BeTrue())
		// The controller's probe mints its own token at the mock's token
		// endpoint before it lists the publisher's models.
		Eventually(func() (string, error) {
			return utils.ResourceField("modelprovider", "", "e2e-vertex-mock",
				`{.status.conditions[?(@.type=="Healthy")].status}`)
		}, "240s", "5s").Should(Equal("True"))
	})

	It("serves generateContent and streamGenerateContent", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("pod", "e2e", "vertex-caller", "{.status.phase}")
		}, "180s", "3s").Should(Equal("Succeeded"))
		logs, err := utils.Kubectl("logs", "-n", "e2e", "vertex-caller")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("generateContent HTTP 200"))
		Expect(logs).To(ContainSubstring("ok from mock"))
		Expect(logs).To(ContainSubstring("streamGenerateContent HTTP 200"))
		stream := logs[strings.Index(logs, "streamGenerateContent HTTP 200"):]
		Expect(stream).To(ContainSubstring("data: "))
		Expect(stream).To(ContainSubstring("usageMetadata"))
	})

	It("meters both calls and reaches the mock once per call", func() {
		out, err := utils.Kubectl("get", "pods", "-n", "kaalm-system",
			"-l", "app.kubernetes.io/component=gateway", "-o", "jsonpath={.items[*].metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		pods := strings.Fields(out)
		Expect(pods).NotTo(BeEmpty())
		var requests, tokens float64
		for _, pod := range pods {
			port, stop, err := utils.PortForwardTarget("kaalm-system", "pod/"+pod, "9090")
			Expect(err).NotTo(HaveOccurred())
			page, err := scrape(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
			stop()
			Expect(err).NotTo(HaveOccurred())
			requests += sumMetric(page, "kaalm_llm_requests_total", `provider="e2e-vertex-mock"`, `status="ok"`)
			tokens += sumMetric(page, "kaalm_llm_tokens_total", `provider="e2e-vertex-mock"`)
		}
		Expect(requests).To(Equal(2.0), "kaalm_llm_requests_total ok for e2e-vertex-mock across replicas")
		Expect(tokens).To(BeNumerically(">", 0), "kaalm_llm_tokens_total for e2e-vertex-mock")

		port, stop, err := utils.PortForward("e2e", "mock-provider", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		Expect(mockRequestCount(port, "/vertex")).To(Equal(2))
	})
})
