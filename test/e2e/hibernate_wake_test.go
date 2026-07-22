//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// pvcUID reads the memory PVC's UID; it is empty (with an error) while the PVC
// is absent. A stable non-empty UID across the hibernate/wake cycle proves the
// same volume persisted and was remounted.
func pvcUID(agent string) (string, error) {
	return utils.ResourceField("pvc", "e2e", agent+"-memory", "{.metadata.uid}")
}

// asyncAccept POSTs an async webhook and returns the 202 requestId.
func asyncAccept(port int, path, bearer string) string {
	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, path)
	body := []byte(`{"userId":"e2e-user","content":{"text":"wake-up"}}`)
	var status int
	var resp string
	var err error
	Eventually(func() (int, error) {
		status, resp, err = utils.PostJSON(url, bearer, body)
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
func pollURL(port int, requestID, channelPath string) string {
	return fmt.Sprintf(
		"https://127.0.0.1:%d/v1/channels/responses/%s?channelPath=%s",
		port, requestID, channelPath)
}

var _ = Describe("Hibernate and wake", Ordered, func() {
	var s7PVCUID string

	BeforeAll(func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/hibernation.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("the hibernation class reconciles to Ready")
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "s7-hibernating")
		}, "60s", "3s").Should(BeTrue())

		By("the S7 agent provisions to Running")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s7-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("its memory PVC exists")
		Eventually(func() (string, error) {
			return pvcUID("s7-agent")
		}, "60s", "3s").ShouldNot(BeEmpty())
		s7PVCUID, _ = pvcUID("s7-agent")
	})

	It("hibernates the idle agent, deleting the Pod but keeping the PVC", func() {
		// Idle and Hibernating are transient; assert the terminal Hibernated
		// state plus the Pod-gone / PVC-kept invariants. The intermediate
		// phase ordering is pinned deterministically by the envtest
		// TestAgent_HibernateAndWake.
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s7-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Hibernated"))

		By("the Pod is gone")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "kaalm.io/agent=s7-agent", "--no-headers")
		}, "60s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))

		By("the PVC survives with the same identity")
		uid, err := pvcUID("s7-agent")
		Expect(err).NotTo(HaveOccurred())
		Expect(uid).To(Equal(s7PVCUID), "the memory PVC must persist across hibernation")

		By("the Service survives and hibernatedAt is stamped")
		_, err = utils.Kubectl("get", "service", "s7-agent", "-n", "e2e")
		Expect(err).NotTo(HaveOccurred())
		hibAt, err := utils.ResourceField("agent", "e2e", "s7-agent", "{.status.hibernatedAt}")
		Expect(err).NotTo(HaveOccurred())
		Expect(hibAt).NotTo(BeEmpty())
	})

	It("wakes on an async webhook and returns the reply by polling", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		requestID := asyncAccept(port, "/channels/e2e/s7-channel", "s7-webhook-bearer-token")

		By("the agent leaves Hibernated (Resuming/Running)")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s7-agent", "{.status.phase}")
		}, "60s", "3s").ShouldNot(Equal("Hibernated"))

		By("the polled response carries the agent's reply")
		// The 180s window absorbs pod recreate plus the kube-router ipset lag
		// on the freshly woken pod before the gateway can deliver.
		var payload string
		Eventually(func() (int, error) {
			var status int
			status, payload, err = utils.GetWithBearer(
				pollURL(port, requestID, "/channels/e2e/s7-channel"), "s7-webhook-bearer-token")
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
		// starter-go echoes the delivered message, proving the full wake ->
		// deliver -> respond round trip.
		Expect(record.Response.Content).To(ContainSubstring("wake-up"))

		By("the memory PVC was remounted, not replaced")
		uid, err := pvcUID("s7-agent")
		Expect(err).NotTo(HaveOccurred())
		Expect(uid).To(Equal(s7PVCUID))
	})

	It("delivers a wake_timeout payload when wakeTimeout is exceeded (S14)", func() {
		By("the S14 agent hibernates")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s14-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Hibernated"))

		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		requestID := asyncAccept(port, "/channels/e2e/s14-channel", "s14-webhook-bearer-token")

		By("the polled record carries a wake_timeout error")
		var payload string
		Eventually(func() (int, error) {
			var status int
			status, payload, err = utils.GetWithBearer(
				pollURL(port, requestID, "/channels/e2e/s14-channel"), "s14-webhook-bearer-token")
			return status, err
		}, "60s", "3s").Should(Equal(200), "poll should return the settled error payload")

		var record struct {
			Error struct {
				Type      string `json:"type"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		Expect(json.Unmarshal([]byte(payload), &record)).To(Succeed())
		Expect(record.Error.Type).To(Equal("wake_timeout"))
		Expect(record.Error.Retryable).To(BeFalse())
	})
})
