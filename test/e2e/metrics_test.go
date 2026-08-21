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
	"fmt"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// The gauges the dashboards plot (#97) are computed on scrape from live
// state, so the proof is the wire: scrape both components after the spend
// and console fixtures have produced state and read the series back.
var _ = Describe("Metric catalog on the wire (#97)", Ordered, func() {
	It("provisions the spend and console fixtures", func() {
		for _, f := range []string{
			"test/e2e/testdata/namespace.yaml",
			"test/e2e/testdata/mockprovider.yaml",
			"test/e2e/testdata/console.yaml",
			"test/e2e/testdata/spend.yaml",
		} {
			_, err := utils.Kubectl("apply", "-f", f)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitRollout("e2e", "mock-provider", "120s")).To(Succeed())
		for _, pod := range []string{"spend-caller-a", "spend-caller-b"} {
			Eventually(func() (string, error) {
				return utils.ResourceField("pod", "console-e2e", pod, "{.status.phase}")
			}, "240s", "5s").Should(Equal("Succeeded"), pod)
		}
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "console-e2e", "console-agent", "{.status.phase}")
		}, "300s", "3s").Should(Equal("Hibernated"))
	})

	It("the controller serves the phase-count gauges from its cache", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-controller", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		Eventually(func() (string, error) {
			return scrape(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		}, "90s", "5s").Should(SatisfyAll(
			ContainSubstring(`kaalm_agents{namespace="console-e2e",phase="Hibernated"} 1`),
			// spend-a and spend-b never idle (their class disables the timers)
			ContainSubstring(`kaalm_agents{namespace="console-e2e",phase="Running"} 2`),
		))
	})

	It("the gateway serves budget utilization from its folded ledger", func() {
		port, stop, err := utils.PortForward("kaalm-system", "kaalm-gateway", "9090")
		Expect(err).NotTo(HaveOccurred())
		defer stop()
		// 45 USD of attested spend against the 100 USD per-namespace ceiling
		// (spend.yaml). Whichever replica answers holds the folded union
		// within one publish interval.
		Eventually(func() (string, error) {
			return scrape(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		}, "90s", "5s").Should(SatisfyAll(
			MatchRegexp(`kaalm_llm_budget_utilization\{namespace="console-e2e",period="[^"]+",provider="console-spend"\} 0\.45`),
			// the latency histogram observes every forwarded request
			ContainSubstring(`kaalm_llm_request_duration_seconds_bucket{`),
		))
	})
})

// scrape fetches a Prometheus exposition page in full: the controller's
// carries the controller-runtime and Go runtime families too, well past the
// 64 KiB the bearer helper caps at.
func scrape(url string) (string, error) {
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("scrape %s: %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}
