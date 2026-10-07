//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kaalm/test/utils"
)

// foreignState holds the parts of a foreign object the controller must leave
// alone. resourceVersion is left out: cert-manager updates a Certificate's
// status on its own. The uid shows the object was not deleted and re-created
// under the same name.
type foreignState struct {
	Metadata struct {
		UID             string            `json:"uid"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
		OwnerReferences []any             `json:"ownerReferences"`
	} `json:"metadata"`
	Spec any               `json:"spec"`
	Type string            `json:"type"`
	Data map[string]string `json:"data"`
}

// readForeign reads one object in the e2e namespace into a foreignState.
func readForeign(kind, name string) (foreignState, error) {
	var st foreignState
	out, err := utils.Kubectl("get", kind, name, "-n", "e2e", "-o", "json")
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, fmt.Errorf("decoding %s %s: %w", kind, name, err)
	}
	return st, nil
}

// A workload's Certificate is named {name}-tls. When a Certificate the
// workload does not control already holds that name, the workload reports
// ChildConflict, starts no Pod, and leaves the Certificate and its Secret
// alone. Envtest covers the ownership logic; only a real cluster has
// cert-manager acting on the foreign Certificate and the controller running
// under its chart RBAC and real informer cache.
var _ = Describe("Workload Certificate name held by a foreign Certificate", Ordered, func() {
	const (
		certKind   = "certificates.cert-manager.io"
		workloads  = "test/e2e/testdata/foreign-certificate-workloads.yaml"
		foreignYML = "test/e2e/testdata/foreign-certificate.yaml"
	)
	type foreignObj struct{ kind, name string }
	foreign := []foreignObj{
		{certKind, "fc-agent-tls"},
		{"secret", "fc-agent-foreign"},
		{certKind, "fc-task-tls"},
		{"secret", "fc-task-foreign"},
	}
	snapshot := map[foreignObj]foreignState{}

	unchanged := func() error {
		for _, o := range foreign {
			got, err := readForeign(o.kind, o.name)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, snapshot[o]) {
				return fmt.Errorf("%s %s changed: was %+v, now %+v", o.kind, o.name, snapshot[o], got)
			}
		}
		return nil
	}

	BeforeAll(func() {
		// Top-level containers run in random order, so this one applies the
		// shared class itself and never deletes it.
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Kubectl("apply", "-f", foreignYML)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", foreignYML, "--ignore-not-found")
			_, _ = utils.Kubectl("delete", "secret", "fc-agent-foreign", "fc-task-foreign",
				"-n", "e2e", "--ignore-not-found")
		})

		By("cert-manager issues both foreign Certificates")
		for _, name := range []string{"fc-agent-tls", "fc-task-tls"} {
			Eventually(func() (bool, error) {
				return readyTrue(certKind, "e2e", name)
			}, "120s", "3s").Should(BeTrue(), "Certificate %s should be Ready", name)
		}
		for _, o := range foreign {
			st, err := readForeign(o.kind, o.name)
			Expect(err).NotTo(HaveOccurred())
			snapshot[o] = st
		}

		// Registered last, so it runs first: the workloads go before the
		// Certificates.
		_, err = utils.Kubectl("apply", "-f", workloads)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Kubectl("delete", "-f", workloads, "--ignore-not-found")
		})
	})

	readyField := func(kind, name, field string) (string, error) {
		return utils.ResourceField(kind, "e2e", name,
			fmt.Sprintf(`{.status.conditions[?(@.type=="Ready")].%s}`, field))
	}

	expectConflict := func(kind, name, cert string) {
		Eventually(func() (string, error) {
			return readyField(kind, name, "reason")
		}, "90s", "3s").Should(Equal("ChildConflict"))
		Expect(readyField(kind, name, "status")).To(Equal("False"))
		Expect(readyField(kind, name, "message")).To(ContainSubstring(fmt.Sprintf("Certificate %q", cert)))
	}

	It("reports ChildConflict on the Agent, naming the Certificate", func() {
		expectConflict("agent", "fc-agent", "fc-agent-tls")
	})

	It("reports ChildConflict on the AgentTask, naming the Certificate", func() {
		expectConflict("agenttask", "fc-task", "fc-task-tls")
	})

	It("creates no Pod and leaves the foreign Certificates and Secrets unchanged across a re-check", func() {
		// 35s is longer than the controller's 30s re-check, so the window
		// covers at least one requeued pass.
		Consistently(func() error {
			for _, w := range []struct{ kind, name, label string }{
				{"agent", "fc-agent", "kaalm.io/agent=fc-agent"},
				{"agenttask", "fc-task", "kaalm.io/task=fc-task"},
			} {
				reason, err := readyField(w.kind, w.name, "reason")
				if err != nil {
					return err
				}
				if reason != "ChildConflict" {
					return fmt.Errorf("%s %s Ready reason is %q", w.kind, w.name, reason)
				}
				phase, err := utils.ResourceField(w.kind, "e2e", w.name, "{.status.phase}")
				if err != nil {
					return err
				}
				if phase != "Pending" {
					return fmt.Errorf("%s %s phase is %q", w.kind, w.name, phase)
				}
				pods, err := utils.Kubectl("get", "pods", "-n", "e2e", "-l", w.label, "-o", "name")
				if err != nil {
					return err
				}
				if strings.TrimSpace(pods) != "" {
					return fmt.Errorf("%s %s has Pods: %s", w.kind, w.name, pods)
				}
			}
			return unchanged()
		}, "35s", "5s").Should(Succeed())
	})

	It("leaves the foreign Certificates and Secrets in place when the workloads are deleted", func() {
		_, err := utils.Kubectl("delete", "-f", workloads, "--ignore-not-found", "--timeout=90s")
		Expect(err).NotTo(HaveOccurred())
		By("the finalizers complete and both workloads are gone")
		Eventually(func() (string, error) {
			out, err := utils.Kubectl("get", "-f", workloads, "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out), err
		}, "90s", "3s").Should(BeEmpty())
		Expect(unchanged()).To(Succeed())
	})
})
