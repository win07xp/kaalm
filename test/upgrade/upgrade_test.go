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

// prevVersion is the released chart the cluster starts from; the Makefile
// pins it and release readiness bumps it each release.
func prevVersion() string {
	if v := os.Getenv("UPGRADE_PREV_VERSION"); v != "" {
		return v
	}
	return "0.6.0"
}

// prevPredatesGraduation reports whether the previous release predates the
// v0.6.0 API graduation. Only such upgrades have the conversion window (the
// old controller serves no webhook yet) and storage to migrate; a
// post-graduation previous release already serves conversion and already
// stores at v1beta1, and the spec asserts that instead. Versions are
// dotted numbers, so the string comparison works within one digit series;
// revisit at 0.10.0.
func prevPredatesGraduation() bool { return prevVersion() < "0.6.0" }

const (
	ns          = "up-e2e"
	channelPath = "/channels/up-e2e/up-channel"
	bearer      = "up-webhook-bearer-token"
)

func helm(args ...string) (string, error) {
	return utils.Run(exec.Command("helm", args...))
}

func phase(kind, name string) string {
	out, _ := utils.Kubectl("get", kind, name, "-n", ns, "-o", "jsonpath={.status.phase}")
	return lastLine(out)
}

// lastLine strips client-go warning lines (deprecation warnings on v1alpha1
// reads land in CombinedOutput) from a jsonpath read.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// keeperPod resolves the keeper's Pod name by its stable labels (agent
// Pods are created with generateName).
func keeperPod() string {
	out, _ := utils.Kubectl("get", "pod", "-n", ns, "-l", "kaalm.io/agent=up-keeper",
		"-o", "jsonpath={.items[0].metadata.name}")
	return lastLine(out)
}

func podUID(pod string) string {
	out, _ := utils.Kubectl("get", "pod", pod, "-n", ns, "-o", "jsonpath={.metadata.uid}")
	return lastLine(out)
}

// The S21 story in order: the old world, the window, the upgrade, and what
// survived. Ordered and stateful on purpose; every later assertion depends
// on the world the earlier steps built.
var _ = Describe("Upgrade in place (S21)", Ordered, func() {
	var keeperPodName, keeperPodUID string

	It("installs the previous release and its workloads at v1alpha1", func() {
		By(fmt.Sprintf("helm install of the released %s chart", prevVersion()))
		_, err := helm("install", "kaalm", "oci://ghcr.io/win07xp/charts/kaalm",
			"--version", prevVersion(), "-n", "kaalm-system", "--create-namespace",
			"--set", "certManager.clusterResourceNamespace=cert-manager",
			"--wait", "--timeout", "5m")
		Expect(err).NotTo(HaveOccurred())

		By("applying the v1alpha1 workloads at the previous release's image versions")
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
	})

	It("opens the window: new CRDs make v1alpha1 reads work; writes match the old release's era", func() {
		By("step 1 of the documented upgrade: apply the new chart's CRDs")
		// --force-conflicts is part of the documented command: helm owns
		// every CRD field from the install, and the first server-side apply
		// must take that ownership over.
		_, err := utils.Kubectl("apply", "--server-side", "--force-conflicts", "-f", "charts/kaalm/crds/")
		Expect(err).NotTo(HaveOccurred())

		By("reads at v1alpha1 still work")
		out, err := utils.Kubectl("get", "agents.v1alpha1.kaalm.io", "up-keeper", "-n", ns,
			"-o", "jsonpath={.metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastLine(out)).To(Equal("up-keeper"))

		if prevPredatesGraduation() {
			By("a write at v1alpha1 fails: storage is v1beta1 and nothing serves the webhook yet")
			out, err = utils.Kubectl("annotate", "agentclasses.v1alpha1.kaalm.io", "up-service",
				"upgrade-e2e/window-probe=first")
			Expect(err).To(HaveOccurred(), "the window write should fail, got: %s", out)
			Expect(out).To(ContainSubstring("conversion webhook"))
		} else {
			By("there is no window: the old release already serves the conversion webhook")
			_, err = utils.Kubectl("annotate", "--overwrite", "agentclasses.v1alpha1.kaalm.io",
				"up-service", "upgrade-e2e/window-probe=first")
			Expect(err).NotTo(HaveOccurred(),
				"a post-graduation release must convert v1alpha1 writes throughout the upgrade")
		}
	})

	It("upgrades the release and the window closes on its own", func() {
		By("step 2 of the documented upgrade: helm upgrade to the local build")
		_, err := helm("upgrade", "kaalm", "charts/kaalm", "-n", "kaalm-system",
			"--set", "certManager.clusterResourceNamespace=cert-manager",
			"--wait", "--timeout", "5m")
		Expect(err).NotTo(HaveOccurred())

		By("the same v1alpha1 write now succeeds, with the deprecation warning")
		var out string
		Eventually(func() error {
			out, err = utils.Kubectl("annotate", "--overwrite", "agentclasses.v1alpha1.kaalm.io",
				"up-service", "upgrade-e2e/window-probe=second")
			return err
		}, "120s", "5s").Should(Succeed())
		Expect(out).To(ContainSubstring("kaalm.io/v1alpha1 AgentClass is deprecated; use kaalm.io/v1beta1"))
	})

	It("kept every workload: nothing recreated, nothing lost", func() {
		By("the keeper is still Running in the same Pod")
		Expect(phase("agent", "up-keeper")).To(Equal("Running"))
		Expect(podUID(keeperPodName)).To(Equal(keeperPodUID),
			"the upgrade must not replace running agent Pods")

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

		By("the chart's sample AgentClass survived: omitted from the upgrade, kept by policy")
		out, err = utils.Kubectl("get", "agentclass", "standard", "-o", "jsonpath={.metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastLine(out)).To(Equal("standard"))
	})

	It("migrated storage: every CRD reports storedVersions [v1beta1]", func() {
		for _, crd := range []string{
			"agentclasses.kaalm.io", "modelproviders.kaalm.io", "toolproviders.kaalm.io",
			"agents.kaalm.io", "agenttasks.kaalm.io", "agentchannels.kaalm.io",
		} {
			Eventually(func() string {
				out, _ := utils.Kubectl("get", "crd", crd, "-o", "jsonpath={.status.storedVersions}")
				return lastLine(out)
			}, "120s", "5s").Should(Equal(`["v1beta1"]`), crd)
		}

		// The pass runs on the new leader a beat after the rollout; with a
		// post-graduation previous release nothing above waited on it, so
		// poll the log for the era-appropriate marker.
		marker := `"kindsAlreadyCurrent":\s*6`
		if prevPredatesGraduation() {
			By("the migrator's log names the kinds it moved")
			marker = `"crd": "agents.kaalm.io", "objects": [1-9]`
		} else {
			By("the migrator found everything already current")
		}
		var logs string
		Eventually(func() string {
			logs, _ = utils.Kubectl("logs", "-n", "kaalm-system",
				"-l", "app.kubernetes.io/component=controller", "--tail=-1", "--prefix")
			return logs
		}, "120s", "5s").Should(MatchRegexp(marker))
		Expect(logs).NotTo(ContainSubstring("storage-version migration failed"))
	})

	It("serves both versions and wakes the sleeper on its next message", func() {
		By("the same object reads back at both versions")
		for _, resource := range []string{"agents.v1alpha1.kaalm.io", "agents.v1beta1.kaalm.io"} {
			out, err := utils.Kubectl("get", resource, "up-sleeper", "-n", ns,
				"-o", "jsonpath={.spec.image}")
			Expect(err).NotTo(HaveOccurred())
			Expect(lastLine(out)).To(Equal("ghcr.io/win07xp/kaalm-agent-python:" + prevVersion()))
		}

		By("a channel message wakes it, as before the upgrade")
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
