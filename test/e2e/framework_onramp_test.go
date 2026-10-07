//go:build e2e

/*
Copyright 2026 The Kaalm Authors.

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

package e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

const (
	s25ChannelPath = "/channels/e2e/s25-channel"
	s25Bearer      = "s25-webhook-bearer-token"
	s25Alice1      = "s25 alice turn 1"
	s25Bob1        = "s25 bob turn 1"
	s25Alice2      = "s25 alice turn 2"
)

// s25Echo mirrors the mock's /echo answer: the number of messages the model
// call carried and the user messages' texts, in order. The langgraph-chat
// handler returns the model's answer as its reply, so this is what the
// LangGraph thread held when the model was called.
type s25Echo struct {
	Messages int      `json:"messages"`
	User     []string `json:"user"`
}

// s25MemoryPVCUID reads the agent's memory PVC UID, which holds the SQLite
// checkpointer. A stable UID across hibernation proves the same volume.
func s25MemoryPVCUID() (string, error) {
	return utils.ResourceField("pvc", "e2e", "s25-langgraph-memory", "{.metadata.uid}")
}

// s25Ask sends one turn as user through the async channel and polls for the
// reply until wait elapses. The POST is never retried: a resend would append
// a second copy of the text to the user's thread.
func s25Ask(port int, user, text string, wait time.Duration) (s25Echo, error) {
	var echo s25Echo
	body, _ := json.Marshal(map[string]any{"content": map[string]string{"text": text}})
	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, s25ChannelPath)
	status, resp, err := utils.PostJSONHeaders(url, s25Bearer, map[string]string{"X-User-Id": user}, body)
	if err != nil {
		return echo, err
	}
	if status != 202 {
		return echo, fmt.Errorf("accept: HTTP %d: %s", status, resp)
	}
	var accepted struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(resp), &accepted); err != nil || accepted.RequestID == "" {
		return echo, fmt.Errorf("accept: no requestId in %s", resp)
	}

	deadline := time.Now().Add(wait)
	for {
		status, payload, err := utils.GetWithBearer(pollURL(port, accepted.RequestID, s25ChannelPath), s25Bearer)
		if err == nil && status == 200 {
			var record struct {
				Response struct {
					Content string `json:"content"`
				} `json:"response"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &record); err != nil {
				return echo, fmt.Errorf("poll record: %w: %s", err, payload)
			}
			if record.Error.Type != "" {
				return echo, fmt.Errorf("delivery failed: %s", payload)
			}
			if err := json.Unmarshal([]byte(record.Response.Content), &echo); err != nil {
				return echo, fmt.Errorf("reply is not the mock's echo: %q", record.Response.Content)
			}
			return echo, nil
		}
		if time.Now().After(deadline) {
			return echo, fmt.Errorf("no reply to %q within %s (last poll: HTTP %d, %v)", text, wait, status, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// S25: an existing framework agent, the langgraph-chat example built FROM the
// Python base image, runs unchanged; its LangGraph thread survives hibernation.
var _ = Describe("Framework on-ramp (S25)", Ordered, func() {
	var pvcUID string

	BeforeAll(func() {
		// Top-level containers run in random order, so this spec brings up the
		// mock itself.
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/mockprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/framework-onramp.yaml")
		Expect(err).NotTo(HaveOccurred())

		By("the class and the provider reconcile to Ready")
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "s25-framework")
		}, "60s", "3s").Should(BeTrue())
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "s25-echo")
		}, "60s", "3s").Should(BeTrue())

		By("the FROM-rung image boots and passes its probes")
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s25-langgraph", "{.status.phase}")
		}, "240s", "5s").Should(Equal("Running"))
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "s25-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))

		By("its memory PVC exists")
		Eventually(func() (string, error) {
			return s25MemoryPVCUID()
		}, "60s", "3s").ShouldNot(BeEmpty())
		pvcUID, _ = s25MemoryPVCUID()
	})

	It("answers through the framework's model client, one thread per user", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		By("a warm-up turn absorbs the fresh Pod's network lag")
		// The kube-router ipset lag on a fresh Pod can fail its first calls
		// for about 20s. Retried deliveries can then repeat a text in a
		// thread, so the lag is spent on a thread nothing asserts.
		Eventually(func() error {
			_, err := s25Ask(port, "s25-warmup", "warm-up", 90*time.Second)
			return err
		}, "300s", "5s").Should(Succeed())

		// Any successful reply also proves that the handler's ChatOpenAI
		// calls carried the Pod's mTLS identity through kaalm.http_client
		// (the gateway rejects a workload call without a client certificate)
		// and that the qualified s25-echo/mock-model routed to the /echo
		// provider.
		By("the first user's first turn starts a one-message thread")
		a1, err := s25Ask(port, "alice", s25Alice1, 120*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(a1).To(Equal(s25Echo{Messages: 1, User: []string{s25Alice1}}))

		By("another user gets a fresh thread")
		b1, err := s25Ask(port, "bob", s25Bob1, 120*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(b1).To(Equal(s25Echo{Messages: 1, User: []string{s25Bob1}}))
	})

	It("hibernates with the Pod gone and the checkpoint volume kept", func() {
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "s25-langgraph", "{.status.phase}")
		}, "300s", "5s").Should(Equal("Hibernated"))

		By("the Pod is gone")
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "kaalm.io/agent=s25-langgraph", "--no-headers")
		}, "60s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))

		By("the PVC survives with the same identity")
		uid, err := s25MemoryPVCUID()
		Expect(err).NotTo(HaveOccurred())
		Expect(uid).To(Equal(pvcUID))
	})

	It("wakes on the user's next message and continues the thread from the volume", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		// The async pipeline wakes the agent and retries delivery through the
		// woken Pod's network lag; the poll waits for the whole cycle.
		r, err := s25Ask(port, "alice", s25Alice2, 300*time.Second)
		Expect(err).NotTo(HaveOccurred())

		// The counts are not exact: on the freshly woken Pod, a delivery
		// retried during the network lag can append the same text twice.
		// The proof is that turn 1 came back from the PVC.
		Expect(r.User).NotTo(BeEmpty())
		Expect(r.User[0]).To(Equal(s25Alice1), "the thread starts with the first user's turn 1")
		Expect(r.User).To(ContainElement(s25Alice2))
		Expect(r.User).To(HaveEach(BeElementOf(s25Alice1, s25Alice2)),
			"no other user's text and no warm-up in the thread")
		Expect(r.Messages).To(BeNumerically(">", len(r.User)),
			"turn 1's assistant reply is in the history")

		By("the memory PVC was remounted, not replaced")
		uid, err := s25MemoryPVCUID()
		Expect(err).NotTo(HaveOccurred())
		Expect(uid).To(Equal(pvcUID))
	})
})
