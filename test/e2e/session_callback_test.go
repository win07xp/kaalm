//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/test/utils"
)

// sessionReply is the starter-go reply shape once it reflects session identity.
type sessionReply struct {
	Content  string `json:"content"`
	Metadata struct {
		UserID    string `json:"userId"`
		SessionID string `json:"sessionId"`
	} `json:"metadata"`
}

// expectedSessionID re-derives the gateway's UUIDv5 session id independently, so
// the assertion checks the derivation formula rather than echoing whatever the
// gateway returned.
func expectedSessionID(channelPath, userID string) string {
	ns := uuid.MustParse(kaalmv1alpha1.SessionNamespaceUUID)
	return uuid.NewSHA1(ns, []byte(channelPath+":"+userID)).String()
}

var _ = Describe("Session identity and async callback", Ordered, func() {
	BeforeAll(func() {
		// e2e-standard is applied by the golden path; re-apply so this spec is
		// self-contained regardless of container order. The mock provides the
		// S15 callback receiver.
		_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())

		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/session_callback.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s12-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s15-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("both channels reconcile to Active (s15's internal callbackUrl passes Rule 22)")
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s12-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s15-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))
	})

	It("derives a stable per-user, distinct-across-users sessionId (S12)", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		url := fmt.Sprintf("https://127.0.0.1:%d/channels/e2e/s12-channel", port)
		send := func(user string) sessionReply {
			var out sessionReply
			Eventually(func() (int, error) {
				status, resp, e := utils.PostJSONHeaders(url, "s12-webhook-bearer-token",
					map[string]string{"X-User-Id": user}, []byte(`{"text":"hi"}`))
				if e != nil || status != 200 {
					return status, e
				}
				return status, json.Unmarshal([]byte(resp), &out)
			}, "60s", "3s").Should(Equal(200))
			return out
		}

		alice1 := send("alice")
		alice2 := send("alice")
		bob := send("bob")

		By("the sessionId is stable across a user's messages")
		Expect(alice1.Metadata.SessionID).NotTo(BeEmpty())
		Expect(alice1.Metadata.SessionID).To(Equal(alice2.Metadata.SessionID))

		By("distinct users get distinct sessionIds")
		Expect(bob.Metadata.SessionID).NotTo(Equal(alice1.Metadata.SessionID))

		By("the sessionId matches the UUIDv5 derivation")
		Expect(alice1.Metadata.SessionID).To(Equal(expectedSessionID("/channels/e2e/s12-channel", "alice")))
		Expect(bob.Metadata.SessionID).To(Equal(expectedSessionID("/channels/e2e/s12-channel", "bob")))
	})

	It("delivers an async reply to the callbackUrl with HMAC headers (S15)", func() {
		gwPort, gwStop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer gwStop()

		url := fmt.Sprintf("https://127.0.0.1:%d/channels/e2e/s15-channel", gwPort)
		var accepted struct {
			RequestID string `json:"requestId"`
		}
		Eventually(func() (int, error) {
			status, resp, e := utils.PostJSON(url, "s15-webhook-bearer-token", []byte(`{"text":"async-hi"}`))
			if e != nil || status != 202 {
				return status, e
			}
			return status, json.Unmarshal([]byte(resp), &accepted)
		}, "60s", "3s").Should(Equal(202))
		Expect(accepted.RequestID).NotTo(BeEmpty())

		By("the callback POST reaches the in-cluster mock, signed and carrying the reply")
		mockPort, mockStop, err := utils.PortForward("e2e", "mock-provider", "8443")
		Expect(err).NotTo(HaveOccurred())
		defer mockStop()

		introspect := fmt.Sprintf("https://127.0.0.1:%d/introspect/callbacks", mockPort)
		Eventually(func() (string, error) {
			_, body, e := utils.GetWithBearer(introspect, "")
			return body, e
		}, "120s", "5s").Should(SatisfyAll(
			ContainSubstring(accepted.RequestID),
			ContainSubstring("X-Kaalm-Signature"),
			ContainSubstring("X-Kaalm-Timestamp"),
			ContainSubstring("starter-go received"),
		))

		By("the polling endpoint recognizes the same requestId as fallback")
		// The callback succeeded, so the async pipeline returned without patching
		// the polling record: the endpoint knows the requestId and reports it
		// pending (202) rather than 404. Polling-serves-the-payload is proven by
		// the S7 wake path (#9).
		poll := fmt.Sprintf("https://127.0.0.1:%d/v1/channels/responses/%s?channelPath=%s",
			gwPort, accepted.RequestID, "/channels/e2e/s15-channel")
		status, _, err := utils.GetWithBearer(poll, "s15-webhook-bearer-token")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(SatisfyAny(Equal(202), Equal(200)))
	})
})
