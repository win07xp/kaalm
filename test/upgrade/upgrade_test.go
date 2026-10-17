//go:build upgrade

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// prevVersion is the released chart the cluster starts from. make
// e2e-upgrade sets it from the Makefile's PREV_CHART_VERSION, the newest
// release tag, so there is no fallback here to drift from it.
func prevVersion() string { return os.Getenv("UPGRADE_PREV_VERSION") }

const (
	ns          = "up-e2e"
	channelPath = "/channels/up-e2e/up-channel"
	bearer      = "up-webhook-bearer-token"
)

var kaalmCRDs = []string{
	"agentclasses.kaalm.io", "modelproviders.kaalm.io", "toolproviders.kaalm.io",
	"agents.kaalm.io", "agenttasks.kaalm.io", "agentchannels.kaalm.io",
}

func helm(args ...string) (string, error) {
	return utils.Run(exec.Command("helm", args...))
}

func phase(kind, name string) string {
	out, _ := utils.Kubectl("get", kind, name, "-n", ns, "-o", "jsonpath={.status.phase}")
	return lastLine(out)
}

// lastLine strips client-go warning lines, which land in CombinedOutput,
// from a jsonpath read.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// readyStatus reads the Ready condition's status of a namespaced object, or
// of a cluster-scoped one when namespace is empty.
func readyStatus(kind, namespace, name string) string {
	args := []string{"get", kind, name, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	out, _ := utils.Kubectl(args...)
	return lastLine(out)
}

// keeperPod resolves the keeper's Pod name by its stable labels (agent
// Pods are created with generateName).
func keeperPod() string {
	out, _ := utils.Kubectl("get", "pod", "-n", ns, "-l", "kaalm.io/agent=up-keeper",
		"-o", "jsonpath={.items[0].metadata.name}")
	return lastLine(out)
}

// certSecretName reads the Secret name a workload's Certificate {name}-tls
// writes (spec.secretName).
func certSecretName(workload string) string {
	out, _ := utils.Kubectl("get", "certificate.cert-manager.io", workload+"-tls", "-n", ns,
		"-o", "jsonpath={.spec.secretName}")
	return lastLine(out)
}

// tlsVolumeSecret reads the Secret a Pod's kaalm-tls projected volume names.
func tlsVolumeSecret(pod string) string {
	out, _ := utils.Kubectl("get", "pod", pod, "-n", ns,
		"-o", `jsonpath={.spec.volumes[?(@.name=="kaalm-tls")].projected.sources[0].secret.name}`)
	return lastLine(out)
}

func podUID(pod string) string {
	out, _ := utils.Kubectl("get", "pod", pod, "-n", ns, "-o", "jsonpath={.metadata.uid}")
	return lastLine(out)
}

// The S21 story in order: the old world, the two documented upgrade steps,
// and what survived. Ordered and stateful on purpose; every later assertion
// depends on the world the earlier steps built.
var _ = Describe("Upgrade in place (S21)", Ordered, func() {
	var keeperPodName, keeperPodUID, keeperSecret string

	It("installs the previous release and its workloads", func() {
		By(fmt.Sprintf("helm install of the released %s chart", prevVersion()))
		_, err := helm("install", "kaalm", "oci://ghcr.io/win07xp/charts/kaalm",
			"--version", prevVersion(), "-n", "kaalm-system", "--create-namespace",
			"--set", "certManager.clusterResourceNamespace=cert-manager",
			"--wait", "--timeout", "5m")
		Expect(err).NotTo(HaveOccurred())

		By("applying the workloads at the previous release's image versions")
		raw, err := os.ReadFile("test/upgrade/testdata/pre-upgrade.yaml")
		Expect(err).NotTo(HaveOccurred())
		rendered := filepath.Join(GinkgoT().TempDir(), "pre-upgrade.yaml")
		Expect(os.WriteFile(rendered,
			[]byte(strings.ReplaceAll(string(raw), "__PREV__", prevVersion())), 0o600)).To(Succeed())
		_, err = utils.Kubectl("apply", "-f", rendered)
		Expect(err).NotTo(HaveOccurred())

		By("the keeper reaches Running (published base image pull included)")
		Eventually(func() string { return phase("agent", "up-keeper") }, "300s", "5s").Should(Equal("Running"))

		By("the task completes through the published runtime's autocomplete")
		Eventually(func() string { return phase("agenttask", "up-task") }, "300s", "5s").Should(Equal("Succeeded"))

		By("the sleeper hibernates")
		Eventually(func() string { return phase("agent", "up-sleeper") }, "180s", "5s").Should(Equal("Hibernated"))

		By("a marker is written to the keeper's memory volume")
		keeperPodName = keeperPod()
		Expect(keeperPodName).NotTo(BeEmpty())
		_, err = utils.Kubectl("exec", "-n", ns, keeperPodName, "--",
			"sh", "-c", "echo survived-the-upgrade > /var/agent/memory/upgrade-marker")
		Expect(err).NotTo(HaveOccurred())
		keeperPodUID = podUID(keeperPodName)
		Expect(keeperPodUID).NotTo(BeEmpty())
		keeperSecret = certSecretName("up-keeper")
		Expect(keeperSecret).NotTo(BeEmpty())
	})

	It("upgrades with the two documented steps", func() {
		By("step 1: apply the new chart's CRDs")
		// --force-conflicts is part of the documented command: helm owns
		// every CRD field from the install, and the first server-side apply
		// must take that ownership over.
		_, err := utils.Kubectl("apply", "--server-side", "--force-conflicts", "-f", "charts/kaalm/crds/")
		Expect(err).NotTo(HaveOccurred())

		By("step 2: helm upgrade to the local build")
		_, err = helm("upgrade", "kaalm", "charts/kaalm", "-n", "kaalm-system",
			"--set", "certManager.clusterResourceNamespace=cert-manager",
			"--wait", "--timeout", "5m")
		Expect(err).NotTo(HaveOccurred())
	})

	It("kept every workload: nothing recreated, nothing lost", func() {
		By("the keeper is still Running and Ready in the same Pod")
		Expect(phase("agent", "up-keeper")).To(Equal("Running"))
		Eventually(func() string { return readyStatus("agent", ns, "up-keeper") }, "120s", "5s").Should(Equal("True"))
		Expect(podUID(keeperPodName)).To(Equal(keeperPodUID),
			"the upgrade must not replace running agent Pods")

		By("the keeper's Certificate keeps its Secret name and the Pod still mounts it")
		Expect(certSecretName("up-keeper")).To(Equal(keeperSecret))
		Expect(tlsVolumeSecret(keeperPodName)).To(Equal(keeperSecret))

		By("the marker is still on the volume")
		out, err := utils.Kubectl("exec", "-n", ns, keeperPodName, "--",
			"cat", "/var/agent/memory/upgrade-marker")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("survived-the-upgrade"))

		By("the finished task still shows Succeeded with its reported status")
		Expect(phase("agenttask", "up-task")).To(Equal("Succeeded"))
		got, err := utils.Kubectl("get", "agenttask", "up-task", "-n", ns,
			"-o", "jsonpath={.status.agentReportedStatus}")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastLine(got)).To(Equal("success"))

		By("the sleeper is still Hibernated")
		Expect(phase("agent", "up-sleeper")).To(Equal("Hibernated"))

		By("the channel and both providers are still Ready")
		Eventually(func() string { return readyStatus("agentchannel", ns, "up-channel") }, "120s", "5s").
			Should(Equal("True"))
		Eventually(func() string { return readyStatus("modelprovider", "", "up-provider") }, "120s", "5s").
			Should(Equal("True"))
		Eventually(func() string { return readyStatus("toolprovider", "", "up-tools") }, "120s", "5s").
			Should(Equal("True"))

		By("the chart's sample AgentClass is still there")
		out, err = utils.Kubectl("get", "agentclass", "standard", "-o", "jsonpath={.metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastLine(out)).To(Equal("standard"))
	})

	It("keeps storage at v1beta1, and the migrator finds every kind current", func() {
		for _, crd := range kaalmCRDs {
			out, err := utils.Kubectl("get", "crd", crd, "-o", "jsonpath={.status.storedVersions}")
			Expect(err).NotTo(HaveOccurred(), crd)
			Expect(lastLine(out)).To(Equal(`["v1beta1"]`), crd)
		}
		// The pass runs on the new leader a beat after the rollout, so poll
		// the log. \s* matches both the JSON encoder and the console one.
		var logs string
		Eventually(func() string {
			logs, _ = utils.Kubectl("logs", "-n", "kaalm-system",
				"-l", "app.kubernetes.io/component=controller", "--tail=-1", "--prefix")
			return logs
		}, "120s", "5s").Should(MatchRegexp(`"kindsAlreadyCurrent":\s*6`))
		Expect(logs).NotTo(ContainSubstring("storage-version migration failed"))
	})

	It("wakes the sleeper on its next message", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)
		requestID := asyncAccept(port, channelPath, bearer)

		var payload string
		Eventually(func() (int, error) {
			var status int
			status, payload, err = utils.GetWithBearer(pollURL(port, requestID, channelPath), bearer)
			return status, err
		}, "180s", "5s").Should(Equal(200), "poll should return the completed response")
		var record struct {
			Response struct {
				Content string `json:"content"`
			} `json:"response"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		Expect(json.Unmarshal([]byte(payload), &record)).To(Succeed())
		Expect(record.Error.Type).To(BeEmpty(), "wake should succeed, not error")
		Expect(record.Response.Content).To(ContainSubstring("upgraded greeter"))
	})
})

// asyncAccept POSTs an async webhook and returns the 202 requestId.
func asyncAccept(port int, path, token string) string {
	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, path)
	body := []byte(`{"userId":"up-user","content":{"text":"wake-up"}}`)
	var resp string
	Eventually(func() (int, error) {
		status, out, err := utils.PostJSON(url, token, body)
		resp = out
		return status, err
	}, "60s", "3s").Should(Equal(202), "async webhook should be accepted with 202")
	var accepted struct {
		RequestID string `json:"requestId"`
	}
	Expect(json.Unmarshal([]byte(resp), &accepted)).To(Succeed())
	Expect(accepted.RequestID).NotTo(BeEmpty())
	return accepted.RequestID
}

// pollURL builds the polling URL for a requestId on a given channel path.
func pollURL(port int, requestID, path string) string {
	return fmt.Sprintf("https://127.0.0.1:%d/v1/channels/responses/%s?channelPath=%s",
		port, requestID, path)
}
