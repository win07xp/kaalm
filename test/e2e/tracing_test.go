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
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// S20: one webhook message, one connected trace. The suite's chart install
// exports to the Jaeger deployed by make e2e-deploy; the trace-agent's
// starter handler answers "ask ..." by calling the model through the
// gateway with the handler's ctx, so the runtime's trace-context
// propagation is what connects the LLM spans to the delivery
// (docs/src/appendix/scenarios.md, S20).
var _ = Describe("Tracing across the hops (S20)", Ordered, func() {
	It("renders no tracing flags on a default install", func() {
		out, err := utils.Helm("template", "kaalm", "charts/kaalm")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("--otlp-endpoint"))
	})

	It("provisions jaeger and the traced fixture", func() {
		for _, f := range []string{
			"test/e2e/testdata/tracing-jaeger.yaml",
			"test/e2e/testdata/namespace.yaml",
			"test/e2e/testdata/mockprovider.yaml",
			"test/e2e/testdata/tracing.yaml",
		} {
			_, err := utils.Kubectl("apply", "-f", f)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitRollout("tracing-e2e", "jaeger", "120s")).To(Succeed())
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "tracing-provider")
		}, "60s", "3s").Should(BeTrue())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "tracing-e2e", "trace-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))
		Eventually(func() (bool, error) {
			return readyTrue("agentchannel", "tracing-e2e", "trace-channel")
		}, "60s", "3s").Should(BeTrue())
	})

	It("one webhook message produces one connected trace", func() {
		gwPort, gwStop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer gwStop()

		// The retry absorbs the freshly-provisioned pod's kube-router ipset
		// lag on both legs (delivery to the agent, agent to the gateway).
		var reply string
		Eventually(func() (int, error) {
			status, body, err := utils.PostJSON(
				fmt.Sprintf("https://127.0.0.1:%d/channels/tracing-e2e/trace-agent", gwPort),
				"trace-webhook-bearer-token", []byte(`{"content":"ask what is 2+2"}`))
			reply = body
			return status, err
		}, "180s", "5s").Should(Equal(200))
		Expect(reply).To(ContainSubstring("ok from mock"))

		jPort, jStop, err := utils.PortForward("tracing-e2e", "jaeger", "16686")
		Expect(err).NotTo(HaveOccurred())
		defer jStop()
		// The batch span processor exports on a schedule; poll until every
		// hop has landed in one connected trace.
		Eventually(func() error {
			return findConnectedTrace(fmt.Sprintf("http://127.0.0.1:%d", jPort),
				"channel.receive", "agent.deliver", "llm.request", "llm.forward")
		}, "90s", "5s").Should(Succeed())
	})
})

// findConnectedTrace queries Jaeger for kaalm-gateway traces touching the
// S20 namespace and demands one trace that contains every wanted operation,
// with each non-root wanted span CHILD_OF another span of the same trace.
func findConnectedTrace(base string, want ...string) error {
	q := url.Values{
		"service": {"kaalm-gateway"},
		"tags":    {`{"kaalm.namespace":"tracing-e2e"}`},
		"limit":   {"20"},
	}
	status, body, err := utils.GetWithBearer(base+"/api/traces?"+q.Encode(), "")
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("jaeger query = %d", status)
	}
	var out struct {
		Data []struct {
			Spans []struct {
				SpanID        string `json:"spanID"`
				OperationName string `json:"operationName"`
				References    []struct {
					RefType string `json:"refType"`
					SpanID  string `json:"spanID"`
				} `json:"references"`
			} `json:"spans"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return fmt.Errorf("jaeger response unreadable: %w", err)
	}
	for _, trace := range out.Data {
		ids := map[string]bool{}
		parent := map[string]string{}
		for _, span := range trace.Spans {
			ids[span.SpanID] = true
			for _, ref := range span.References {
				if ref.RefType == "CHILD_OF" {
					parent[span.OperationName] = ref.SpanID
				}
			}
			parent[span.OperationName+"/id"] = span.SpanID
		}
		complete := true
		for _, op := range want {
			if !ids[parent[op+"/id"]] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		// channel.receive is the root; every other wanted span must hang off
		// a span inside this same trace.
		connected := true
		for _, op := range want[1:] {
			if !ids[parent[op]] {
				connected = false
				break
			}
		}
		if connected {
			return nil
		}
	}
	return fmt.Errorf("no connected trace with %v yet (%d candidates)", want, len(out.Data))
}
