//go:build e2e

package e2e

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// kataRuntimeClass is the RuntimeClass hack/k3d-up.sh creates with KATA=1.
const kataRuntimeClass = "kata-qemu-runtime-rs"

// MicroVM isolation: the class runs its workloads under Kata, so each Pod is
// a QEMU guest. The spec checks the places where a VM boundary could differ
// from a container: the guest kernel, the PVC and TLS volumes shared over
// virtio-fs, the kubelet probes, NetworkPolicy at the host CNI, the Pod
// overhead, and the wake time. It runs only where the Kata RuntimeClass
// exists (make e2e-kata).
var _ = Describe("MicroVM isolation on Kata", Ordered, func() {
	var nodeKernel string

	BeforeAll(func() {
		if _, err := utils.Kubectl("get", "runtimeclass", kataRuntimeClass); err != nil {
			Skip("no " + kataRuntimeClass + " RuntimeClass")
		}
		var err error
		nodeKernel, err = utils.Kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.kernelVersion}")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/microvm.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", "test/e2e/testdata/microvm.yaml", "--ignore-not-found")
		})
	})

	It("runs the agent in a Kata Pod that passes its probes and carries the overhead", func() {
		// Running needs the Pod Ready, so the HTTPS readiness probe reached the
		// agent inside the guest, and the agent served with the projected TLS
		// files.
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "mv-agent", "{.status.phase}")
		}, "300s", "5s").Should(Equal("Running"))
		Expect(agentPodField("mv-agent", "{.items[0].spec.runtimeClassName}")).To(Equal(kataRuntimeClass))
		// The RuntimeClass overhead lands on the Pod beside the class's
		// container defaults, which it does not change.
		Expect(agentPodField("mv-agent", "{.items[0].spec.overhead.memory}")).To(Equal("320Mi"))
		Expect(agentPodField("mv-agent", "{.items[0].spec.containers[0].resources.limits.memory}")).To(Equal("512Mi"))
	})

	It("enforces the agent's NetworkPolicy at the host CNI", func() {
		probe := []string{
			"run", "mv-deny-probe", "-n", "default", "--rm", "-i", "--restart=Never",
			"--image=curlimages/curl:8.23.0", "--image-pull-policy=IfNotPresent",
			"--command", "--",
			"sh", "-c",
			"curl -sk --max-time 6 -o /dev/null -w '%{http_code}' " +
				"https://mv-agent.e2e.svc.cluster.local:8080/readyz || true",
		}
		Eventually(func() (string, error) {
			return utils.Kubectl(probe...)
		}, "60s", "5s").Should(ContainSubstring("000"))
	})

	It("runs a task in its own kernel with its PVC and TLS files", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agenttask", "e2e", "mv-task", "{.status.phase}")
		}, "300s", "5s").Should(Equal("Succeeded"))
		out, err := utils.ResourceField("agenttask", "e2e", "mv-task", "{.status.artifactValues}")
		Expect(err).NotTo(HaveOccurred())
		var seen map[string]string
		Expect(json.Unmarshal([]byte(out), &seen)).To(Succeed())
		AddReportEntry("guest", seen)
		AddReportEntry("node kernel", nodeKernel)
		Expect(seen["kernel"]).NotTo(BeEmpty())
		Expect(seen["kernel"]).NotTo(Equal(nodeKernel), "the task should boot its own guest kernel")
		Expect(seen["memory"]).To(Equal("written"))
		Expect(seen["tls"]).To(Equal("readable"))
	})

	It("hibernates and wakes the agent, timed against the default runtime", func() {
		for _, name := range []string{"mv-sleeper", "mv-sleeper-runc"} {
			Eventually(func() (string, error) {
				return utils.ResourceField("agent", "e2e", name, "{.status.phase}")
			}, "300s", "5s").Should(Equal("Hibernated"), name)
		}

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		wake := func(name string) time.Duration {
			path := "/channels/e2e/" + name
			start := time.Now()
			requestID := asyncAccept(port, path, "mv-webhook-bearer-token")
			var payload string
			Eventually(func() (int, error) {
				status, body, err := utils.GetWithBearer(pollURL(port, requestID, path), "mv-webhook-bearer-token")
				payload = body
				return status, err
			}, "240s", "2s").Should(Equal(200), name)
			took := time.Since(start)
			var record struct {
				Response struct {
					Content string `json:"content"`
				} `json:"response"`
			}
			Expect(json.Unmarshal([]byte(payload), &record)).To(Succeed())
			Expect(record.Response.Content).To(ContainSubstring("wake-up"), name)
			return took
		}
		kata := wake("mv-sleeper")
		runc := wake("mv-sleeper-runc")
		AddReportEntry("wake to reply", map[string]string{
			"kata": kata.Round(time.Second).String(),
			"runc": runc.Round(time.Second).String(),
		})
	})
})
