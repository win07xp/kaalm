# k3d e2e Suite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Gate the real Helm deploy artifact end to end on an ephemeral k3d cluster: chart install to a running Agent Pod, a synchronous webhook that returns the agent's reply, the per-agent NetworkPolicy boundary, and an AgentTask that runs to completion.

**Architecture:** A Ginkgo/Gomega suite behind the `//go:build e2e` tag installs `charts/agentry` onto the k3d cluster created by `hack/k3d-up.sh`, then drives CRs through their lifecycles and asserts observed status. A single agent image built from `examples/starter-go` serves both the echo (channel) and task paths; task completion is triggered by a new env-gated hook. `make e2e` is the one-shot that both local dev and CI invoke.

**Tech Stack:** Go 1.24+, Ginkgo v2/Gomega, k3d, Helm, kubectl, cert-manager + trust-manager, controller-runtime CRDs.

## Global Constraints

- k3d cluster name `agentry-dev`; operator namespace `agentry-system`; trust namespace `cert-manager` (`certManager.clusterResourceNamespace=cert-manager`).
- Chart appVersion `0.1.0`; controller/gateway images `ghcr.io/win07xp/agentry-controller:0.1.0` and `ghcr.io/win07xp/agentry-gateway:0.1.0`.
- Agent runtime image `registry.test/agents/starter-go:e2e`; AgentClass `allowedImages: registry.test/agents/*`; `imagePullPolicy: IfNotPresent` (image is `k3d image import`ed, never pulled).
- All e2e Go files carry `//go:build e2e` EXCEPT `test/utils/utils.go` (untagged; imported by tagged files).
- Status constants (verbatim): Agent phase `Running`, AgentChannel phase `Active`, AgentTask phase `Succeeded`; AgentClass/ModelProvider readiness via `status.conditions[type=Ready].status == True`.
- Gateway user (webhook) listener: Service `agentry-gateway` port `8080`, scheme HTTPS.
- Docs/README copy: no em-dashes or en-dashes.
- Commit messages end with a trailer line: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Work stays on branch `feat/go-scaffold`. Do not push (the user pushes).
- The k3d cluster `agentry-dev` is assumed already up with the chart installed during development; `make e2e` is idempotent and re-establishes everything from scratch on a clean machine.

---

### Task 1: Env-gated task auto-complete in starter-go

The stock `examples/starter-go` detects task mode from its cert SAN but never calls `completeTask` (the README leaves that to the user). Add a small hook so the same image self-reports completion in task mode when `AGENTRY_TASK_AUTOCOMPLETE` is set. Default behaviour (env unset) is unchanged.

**Files:**
- Modify: `examples/starter-go/main.go`
- Modify: `examples/starter-go/main_test.go`
- Modify: `examples/starter-go/README.md`

**Interfaces:**
- Produces: `taskAutocompleteStatus(isTask bool, env string) string` — returns the status to self-report on startup (`""` disables). The e2e AgentTask sets `AGENTRY_TASK_AUTOCOMPLETE=success` via `spec.env` (Task 3).

- [ ] **Step 1: Write the failing test**

Add to `examples/starter-go/main_test.go`:

```go
func TestTaskAutocompleteStatus(t *testing.T) {
	if got := taskAutocompleteStatus(false, "success"); got != "" {
		t.Errorf("agent mode must never auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, ""); got != "" {
		t.Errorf("unset env must not auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, "success"); got != "success" {
		t.Errorf("task mode with env must return the status, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd examples/starter-go && go test ./... -run TestTaskAutocompleteStatus`
Expected: FAIL (`undefined: taskAutocompleteStatus`).

- [ ] **Step 3: Add the helper and wire it in `main.go`**

Add this function near the bottom of `examples/starter-go/main.go`:

```go
// taskAutocompleteStatus returns the status an AgentTask should self-report on
// startup, or "" to disable. Honored only in task mode; the value comes from
// AGENTRY_TASK_AUTOCOMPLETE ("success" or "failure"). This is a smoke/e2e hook:
// a real task reports completion from its own work, not on startup.
func taskAutocompleteStatus(isTask bool, env string) string {
	if !isTask {
		return ""
	}
	return env
}
```

In `main.go`, immediately after the server-start goroutine and before `<-ctx.Done()`, insert:

```go
	if status := taskAutocompleteStatus(a.isTask, os.Getenv("AGENTRY_TASK_AUTOCOMPLETE")); status != "" {
		go func() {
			if err := a.completeTask(ctx, status, "auto-complete on startup", nil); err != nil {
				log.Printf("task auto-complete failed: %v", err)
			} else {
				log.Printf("task auto-complete reported %q", status)
			}
		}()
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd examples/starter-go && go test ./... && go build .`
Expected: PASS and a clean build.

- [ ] **Step 5: Document the hook in the README**

In `examples/starter-go/README.md`, under the AgentTask section, add:

```markdown
For smoke and e2e runs, set `AGENTRY_TASK_AUTOCOMPLETE=success` (via the
AgentTask `spec.env`) to have the task report that status on startup through
`completeTask`. Leave it unset in real tasks, which report completion from
their own work.
```

- [ ] **Step 6: Commit**

```bash
git add examples/starter-go/main.go examples/starter-go/main_test.go examples/starter-go/README.md
git commit -m "Add env-gated task auto-complete hook to starter-go

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: k3d/helm/e2e helpers in test/utils

Replace the stale Kind/Prometheus helpers with the exec and HTTP helpers the suite needs. Keep `Run`, `GetNonEmptyLines`, `GetProjectDir`.

**Files:**
- Modify: `test/utils/utils.go`

**Interfaces:**
- Produces:
  - `Kubectl(args ...string) (string, error)`
  - `Helm(args ...string) (string, error)`
  - `WaitRollout(namespace, deploy, timeout string) error`
  - `ResourceField(kind, namespace, name, jsonpath string) (string, error)`
  - `PortForward(namespace, svc, remotePort string) (localPort int, stop func(), err error)`
  - `PostJSON(url, bearer string, body []byte) (status int, respBody string, err error)`
  - `parseForwardPort(line string) (int, bool)` (pure, unit-tested)

- [ ] **Step 1: Write the failing test**

Create `test/utils/utils_test.go`:

```go
package utils

import "testing"

func TestParseForwardPort(t *testing.T) {
	p, ok := parseForwardPort("Forwarding from 127.0.0.1:34567 -> 8080")
	if !ok || p != 34567 {
		t.Fatalf("got (%d,%v), want (34567,true)", p, ok)
	}
	if _, ok := parseForwardPort("Forwarding from [::1]:34567 -> 8080"); ok {
		t.Error("must only match the IPv4 line to avoid a duplicate port")
	}
	if _, ok := parseForwardPort("random log line"); ok {
		t.Error("non-matching line must return false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./test/utils/ -run TestParseForwardPort`
Expected: FAIL (`undefined: parseForwardPort`).

- [ ] **Step 3: Replace the helper body of `test/utils/utils.go`**

Keep the license header, `package utils`, `Run`, `warnError`, `GetNonEmptyLines`, and `GetProjectDir`. Delete `InstallPrometheusOperator`, `UninstallPrometheusOperator`, `IsPrometheusCRDsInstalled`, `InstallCertManager`, `UninstallCertManager`, `IsCertManagerCRDsInstalled`, `LoadImageToKindClusterWithName`, `UncommentCode`, and the `prometheus*`/`certmanager*` constants. Set the import block to:

```go
import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
)
```

Add these functions:

```go
// Kubectl runs kubectl with the given args from the project root.
func Kubectl(args ...string) (string, error) {
	return Run(exec.Command("kubectl", args...))
}

// Helm runs helm with the given args from the project root.
func Helm(args ...string) (string, error) {
	return Run(exec.Command("helm", args...))
}

// WaitRollout blocks until a Deployment's rollout completes or the timeout fires.
func WaitRollout(namespace, deploy, timeout string) error {
	_, err := Kubectl("rollout", "status", "deploy/"+deploy, "-n", namespace, "--timeout", timeout)
	return err
}

// ResourceField reads a single jsonpath field off a resource. namespace may be
// "" for cluster-scoped kinds.
func ResourceField(kind, namespace, name, jsonpath string) (string, error) {
	args := []string{"get", kind, name, "-o", "jsonpath=" + jsonpath}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return Kubectl(args...)
}

// parseForwardPort extracts the local port kubectl prints for a dynamic
// port-forward. It matches only the IPv4 line so the IPv6 duplicate is ignored.
func parseForwardPort(line string) (int, bool) {
	const marker = "Forwarding from 127.0.0.1:"
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(marker):]
	j := strings.IndexByte(rest, ' ')
	if j < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return p, true
}

// PortForward starts `kubectl port-forward svc/<svc> :<remotePort>` with a
// kubectl-chosen local port (avoids collisions), returning the local port and a
// stop func. Caller must call stop().
func PortForward(namespace, svc, remotePort string) (int, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, "svc/"+svc, ":"+remotePort)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	portCh := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if p, ok := parseForwardPort(scanner.Text()); ok {
				portCh <- p
				return
			}
		}
		portCh <- 0
	}()

	select {
	case p := <-portCh:
		if p == 0 {
			stop()
			return 0, nil, fmt.Errorf("port-forward did not report a local port")
		}
		return p, stop, nil
	case <-time.After(15 * time.Second):
		stop()
		return 0, nil, fmt.Errorf("port-forward timed out waiting for a local port")
	}
}

// PostJSON POSTs a JSON body over HTTPS, skipping cert verification (the target
// is a localhost port-forward in the e2e suite only).
func PostJSON(url, bearer string, body []byte) (int, string, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // localhost port-forward, e2e only
	cli := &http.Client{Timeout: 45 * time.Second, Transport: tr}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(b), nil
}
```

- [ ] **Step 4: Run test and build to verify**

Run: `go test ./test/utils/ -run TestParseForwardPort && go build ./test/utils/`
Expected: PASS and a clean build.

- [ ] **Step 5: Commit**

```bash
git add test/utils/utils.go test/utils/utils_test.go
git commit -m "Replace Kind e2e helpers with k3d/helm/port-forward helpers

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: e2e testdata fixtures

Author the CRs the suite applies. Cluster-scoped kinds (AgentClass, ModelProvider) plus namespaced workloads in the `e2e` namespace. All images are `registry.test/agents/*`.

**Files:**
- Create: `test/e2e/testdata/namespace.yaml`
- Create: `test/e2e/testdata/secrets.yaml`
- Create: `test/e2e/testdata/agentclass.yaml`
- Create: `test/e2e/testdata/modelprovider.yaml`
- Create: `test/e2e/testdata/agent.yaml`
- Create: `test/e2e/testdata/agentchannel.yaml`
- Create: `test/e2e/testdata/agenttask.yaml`

**Interfaces:**
- Produces: named objects consumed by Tasks 5-7: namespace `e2e`; Secrets `e2e-openai-key` and `e2e-hook` (both in `e2e`); AgentClass `e2e-standard`; ModelProvider `e2e-openai`; Agent `e2e-agent`; AgentChannel `e2e-channel` (webhook path `/channels/e2e/e2e-channel`, bearer token in `e2e-hook` key `token`); AgentTask `e2e-task`.

- [ ] **Step 1: Write the fixtures**

`test/e2e/testdata/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: e2e
```

`test/e2e/testdata/secrets.yaml`:

```yaml
# ModelProvider credential Secrets are resolved in the operator namespace
# (modelprovider_controller.go reads credentialsRef from OperatorNamespace),
# so this one lives in agentry-system, not e2e.
apiVersion: v1
kind: Secret
metadata:
  name: e2e-vertex-key
  namespace: agentry-system
type: Opaque
stringData:
  token: dummy-not-used-probe-skipped
---
# The channel webhook bearer Secret lives alongside the AgentChannel in e2e;
# the reconciler grants the gateway a scoped read there.
apiVersion: v1
kind: Secret
metadata:
  name: e2e-hook
  namespace: e2e
type: Opaque
stringData:
  token: e2e-webhook-bearer-token
```

`test/e2e/testdata/agentclass.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentClass
metadata:
  name: e2e-standard
spec:
  runtime:
    backend: pod
  image:
    allowedImages: ["registry.test/agents/*"]
    pullPolicy: IfNotPresent
  lifecycle:
    defaultIdleTimeout: 0s
    defaultHibernationDelay: 0s
    defaultWakeTimeout: 0s
    maxIdleTimeout: 0s
    maxHibernationDelay: 0s
    maxWakeTimeout: 0s
```

`test/e2e/testdata/modelprovider.yaml`:

```yaml
# Hermetic e2e: google-vertex has no liveness probe implemented, so Probe
# returns Skipped (no network) and Ready goes True on a dummy credential.
# An openai-type provider would run a real authenticated health probe that
# 401s on the dummy credential and blocks Ready; healthCheck.enabled:false
# cannot currently disable it (the field is `bool,omitempty`, so false is
# dropped and reconcile-time defaulting re-enables the probe).
apiVersion: agentry.io/v1alpha1
kind: ModelProvider
metadata:
  name: e2e-vertex
spec:
  type: google-vertex
  endpoint: https://us-central1-aiplatform.googleapis.com
  credentialsRef:
    name: e2e-vertex-key
    key: token
  models:
    - id: gemini-1.5-pro
      costPer1MInputTokens: "1.25"
      costPer1MOutputTokens: "5.00"
```

`test/e2e/testdata/agent.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
kind: Agent
metadata:
  name: e2e-agent
  namespace: e2e
spec:
  agentClassRef:
    name: e2e-standard
  image: registry.test/agents/starter-go:e2e
  lifecycle:
    activitySource: gatewayTraffic
    hibernationDelay: 0s
    idleTimeout: 0s
    wakeTimeout: 0s
```

`test/e2e/testdata/agentchannel.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentChannel
metadata:
  name: e2e-channel
  namespace: e2e
spec:
  agentRef:
    name: e2e-agent
  type: webhook
  webhook:
    path: /channels/e2e/e2e-channel
    responseMode: sync
    auth:
      type: bearer
      secretRef:
        name: e2e-hook
        key: token
```

`test/e2e/testdata/agenttask.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentTask
metadata:
  name: e2e-task
  namespace: e2e
spec:
  agentClassRef:
    name: e2e-standard
  image: registry.test/agents/starter-go:e2e
  env:
    - name: AGENTRY_TASK_AUTOCOMPLETE
      value: success
  completion:
    condition: agentReported
    timeout: 2m
    onTimeout: Fail
  ttlSecondsAfterFinished: 30
```

- [ ] **Step 2: Validate every fixture against the live CRDs**

Run (requires the `agentry-dev` cluster up with CRDs installed):

```bash
kubectl apply --dry-run=server -f test/e2e/testdata/namespace.yaml
kubectl create ns e2e --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1 || true
for f in secrets agentclass modelprovider agent agentchannel agenttask; do
  kubectl apply --dry-run=server -f test/e2e/testdata/$f.yaml
done
```

Expected: every file reports `... configured (server dry run)` or `created (server dry run)` with no CEL/schema rejection. Fix any field the API server rejects (enum, required, mutex rules) before proceeding.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/testdata/
git commit -m "Add e2e testdata fixtures (class, provider, agent, channel, task)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Makefile `e2e` one-shot and drop the stale Kind target

Rework the `e2e` target into an idempotent one-shot that brings up the cluster, builds and imports the three images, installs the chart, and runs the suite. Remove the stale kubebuilder Kind `test-e2e` target.

**Files:**
- Modify: `Makefile`

**Interfaces:**
- Produces: `make e2e`, `make e2e-images`, `make e2e-deploy`. Consumed by Task 8 (CI) and local runs.

- [ ] **Step 1: Read the current e2e-related targets**

Run: `grep -nE "e2e|kind|KIND|test-e2e|CHART_DIR|chart-sync" Makefile`
Note the exact lines for `test-e2e`, `e2e`, `KIND ?=`, and `chart-sync` so the edits below land in the right place.

- [ ] **Step 2: Replace the `e2e` target and remove `test-e2e`**

Delete the entire `test-e2e:` target block (the kubebuilder one that greps for a `kind` cluster) and the `KIND ?= kind` variable if nothing else references it. Add near the existing `k3d-up`/`e2e` block:

```makefile
CLUSTER ?= agentry-dev
CHART_APP_VERSION := $(shell grep '^appVersion:' charts/agentry/Chart.yaml | awk '{print $$2}' | tr -d '"')
CONTROLLER_IMG ?= ghcr.io/win07xp/agentry-controller:$(CHART_APP_VERSION)
GATEWAY_IMG ?= ghcr.io/win07xp/agentry-gateway:$(CHART_APP_VERSION)
AGENT_IMG ?= registry.test/agents/starter-go:e2e

.PHONY: e2e-images
e2e-images: ## Build the controller, gateway, and agent images and import them into k3d.
	docker build -t $(CONTROLLER_IMG) --build-arg BINARY=manager .
	docker build -t $(GATEWAY_IMG) --build-arg BINARY=gateway .
	docker build -t $(AGENT_IMG) examples/starter-go
	k3d image import $(CONTROLLER_IMG) $(GATEWAY_IMG) $(AGENT_IMG) -c $(CLUSTER)

.PHONY: e2e-deploy
e2e-deploy: chart-sync ## Install/upgrade the chart onto the current context.
	helm upgrade --install agentry charts/agentry -n agentry-system --create-namespace \
		--set certManager.clusterResourceNamespace=cert-manager --wait --timeout 5m

.PHONY: e2e
e2e: ## One-shot k3d e2e: bring up the cluster, build+import images, install the chart, run the suite.
	hack/k3d-up.sh
	$(MAKE) e2e-images
	$(MAKE) e2e-deploy
	go test ./test/e2e/... -tags e2e -v -timeout 20m
```

If an old `e2e:` target already exists (the `-tags e2e` runner), replace it wholesale with the block above.

- [ ] **Step 3: Verify the wiring dry-runs and the images build+import**

Run:

```bash
make -n e2e            # prints the four ordered steps, no errors
make e2e-images        # builds 3 images and imports them into agentry-dev
make e2e-deploy        # helm upgrade --install succeeds, pods Ready
```

Expected: `make -n e2e` lists `hack/k3d-up.sh`, `make e2e-images`, `make e2e-deploy`, and the `go test` line. `e2e-images` ends with `Successfully imported image(s)`. `e2e-deploy` ends with the release deployed and `--wait` returning success.

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "Rework the e2e Make target into a k3d one-shot; drop the stale Kind target

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: e2e suite bootstrap (Before/AfterSuite)

Rewrite the stale kbinit suite file. `BeforeSuite` asserts the chart is deployed and seeds the namespace and secrets; `AfterSuite` tears down the e2e objects (leaving the chart and cluster). Add one deployment sanity spec so the suite runs green before the lifecycle specs land.

**Files:**
- Modify (rewrite): `test/e2e/e2e_suite_test.go`

**Interfaces:**
- Consumes: `utils.WaitRollout`, `utils.Kubectl` (Task 2); `test/e2e/testdata/*` (Task 3).
- Produces: the `e2e` Ginkgo suite entrypoint `TestE2E`; the namespace/secrets seeded for Tasks 6-7.

- [ ] **Step 1: Replace the file contents**

Overwrite `test/e2e/e2e_suite_test.go` with:

```go
//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting agentry e2e suite\n")
	RunSpecs(t, "agentry e2e suite")
}

var _ = BeforeSuite(func() {
	By("verifying the controller and gateway rollouts are Ready")
	Expect(utils.WaitRollout("agentry-system", "agentry-controller", "150s")).To(Succeed())
	Expect(utils.WaitRollout("agentry-system", "agentry-gateway", "150s")).To(Succeed())

	By("seeding the e2e namespace and secrets")
	_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/namespace.yaml")
	Expect(err).NotTo(HaveOccurred())
	_, err = utils.Kubectl("apply", "-f", "test/e2e/testdata/secrets.yaml")
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	By("tearing down e2e objects (chart and cluster are left in place)")
	// Order: workloads first, then cluster-scoped, then the namespace.
	for _, f := range []string{
		"test/e2e/testdata/agenttask.yaml",
		"test/e2e/testdata/agentchannel.yaml",
		"test/e2e/testdata/agent.yaml",
		"test/e2e/testdata/modelprovider.yaml",
		"test/e2e/testdata/agentclass.yaml",
		"test/e2e/testdata/secrets.yaml",
		"test/e2e/testdata/namespace.yaml",
	} {
		_, _ = utils.Kubectl("delete", "-f", f, "--ignore-not-found", "--wait=false")
	}
})

var _ = Describe("Deployment", func() {
	It("has all five Agentry CRDs installed", func() {
		out, err := utils.Kubectl("get", "crds", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		for _, crd := range []string{
			"agentclasses.agentry.io", "modelproviders.agentry.io", "agents.agentry.io",
			"agenttasks.agentry.io", "agentchannels.agentry.io",
		} {
			Expect(out).To(ContainSubstring(crd))
		}
	})
})
```

- [ ] **Step 2: Run the suite to verify the bootstrap passes**

Run: `make e2e` (or, if the cluster and chart are already up: `go test ./test/e2e/... -tags e2e -v -timeout 20m`)
Expected: the `Deployment` spec passes; BeforeSuite/AfterSuite run without error.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/e2e_suite_test.go
git commit -m "Rewrite the e2e suite bootstrap for the agentry chart on k3d

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Golden-path specs (1-6)

The core spine: class and provider Ready, Agent Running, Channel Active, sync webhook returns the agent's reply, netpol blocks a disallowed source.

**Files:**
- Create: `test/e2e/golden_path_test.go`

**Interfaces:**
- Consumes: `utils.Kubectl`, `utils.ResourceField`, `utils.PortForward`, `utils.PostJSON` (Task 2); `test/e2e/testdata/*` (Task 3); the `e2e` namespace and `e2e-hook` secret seeded in BeforeSuite (Task 5).

- [ ] **Step 1: Write the golden-path specs**

Create `test/e2e/golden_path_test.go`:

```go
//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

// readyTrue reports whether a resource's Ready condition is True.
func readyTrue(kind, namespace, name string) (bool, error) {
	out, err := utils.ResourceField(kind, namespace, name,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	if err != nil {
		return false, err
	}
	return out == "True", nil
}

var _ = Describe("Golden path", Ordered, func() {
	It("reconciles the AgentClass to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("agentclass", "", "e2e-standard")
		}, "60s", "3s").Should(BeTrue())
	})

	It("reconciles the ModelProvider to Ready", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/modelprovider.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			return readyTrue("modelprovider", "", "e2e-vertex")
		}, "60s", "3s").Should(BeTrue())
	})

	It("provisions the Agent Pod to Running with its child resources", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agent.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agent", "e2e", "e2e-agent", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Running"))

		By("the per-agent Service, NetworkPolicy, and ServiceAccount exist")
		_, err = utils.Kubectl("get", "service", "e2e-agent", "-n", "e2e")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Kubectl("get", "networkpolicy", "e2e-agent", "-n", "e2e")
		Expect(err).NotTo(HaveOccurred())
	})

	It("reconciles the AgentChannel to Active", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agentchannel.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agentchannel", "e2e", "e2e-channel", "{.status.phase}")
		}, "90s", "3s").Should(Equal("Active"))
	})

	It("delivers a sync webhook and returns the agent's reply", func() {
		port, stop, err := utils.PortForward("agentry-system", "agentry-gateway", "8080")
		Expect(err).NotTo(HaveOccurred())
		defer stop()

		url := fmt.Sprintf("https://127.0.0.1:%d/channels/e2e/e2e-channel", port)
		body := []byte(`{"userId":"e2e-user","content":{"text":"ping"}}`)

		var status int
		var resp string
		Eventually(func() (int, error) {
			status, resp, err = utils.PostJSON(url, "e2e-webhook-bearer-token", body)
			return status, err
		}, "60s", "3s").Should(Equal(200))

		// The starter-go agent echoes the delivered message back in its reply,
		// so the reply content must contain the text we sent ("ping"). This
		// proves the full round trip without pinning the template's phrasing.
		var reply struct {
			Content string `json:"content"`
		}
		Expect(json.Unmarshal([]byte(resp), &reply)).To(Succeed())
		Expect(reply.Content).To(ContainSubstring("ping"))
	})

	It("blocks delivery from a disallowed namespace via the NetworkPolicy", func() {
		// A pod in `default` is not in the agent's ingress allow-list, so the
		// synthesized NetworkPolicy must refuse it. (The allowed gateway path is
		// already proven by the sync-webhook spec above.)
		probe := []string{
			"run", "np-deny-probe", "-n", "default", "--rm", "-i", "--restart=Never",
			"--image=curlimages/curl:8.10.1", "--command", "--",
			"sh", "-c",
			"curl -sk --max-time 6 -o /dev/null -w '%{http_code}' " +
				"https://e2e-agent.e2e.svc.cluster.local:8080/readyz || true",
		}
		Eventually(func() (string, error) {
			return utils.Kubectl(probe...)
		}, "60s", "5s").ShouldNot(ContainSubstring("200"))
	})
})
```

- [ ] **Step 2: Run the golden-path specs**

Run: `go test ./test/e2e/... -tags e2e -v -timeout 20m -args -ginkgo.focus="Golden path"`
Expected: all six specs pass. The sync-webhook spec returns 200 with `content` containing `echo:`; the netpol spec never sees `200`.

- [ ] **Step 3: If the netpol probe flakes, confirm the deny is stable**

If the probe intermittently sees `200`, the agent netpol may not yet target the pod. Verify it exists and re-run:

```bash
kubectl get networkpolicy e2e-agent -n e2e -o yaml
```
Expected: `podSelector` matches `agentry.io/agent: e2e-agent`; ingress allows only the gateway. The deny is stable once this exists (it predates the probe). No code change if present.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/golden_path_test.go
git commit -m "Add golden-path e2e specs (class, provider, agent, channel, webhook, netpol)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Task-lifecycle specs (7-8)

An `agentReported` AgentTask runs to Succeeded via the auto-complete hook, gets its completion mailbox and per-task Role, and is garbage-collected after its TTL.

**Files:**
- Create: `test/e2e/task_lifecycle_test.go`

**Interfaces:**
- Consumes: `utils.Kubectl`, `utils.ResourceField` (Task 2); `testdata/agenttask.yaml` (Task 3); Agent/Class already applied by the golden path or applied here if run in isolation.

- [ ] **Step 1: Write the task-lifecycle specs**

Create `test/e2e/task_lifecycle_test.go`:

```go
//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/win07xp/kubeclaw/test/utils"
)

var _ = Describe("Task lifecycle", Ordered, func() {
	BeforeAll(func() {
		// Ensure the class exists even when this spec runs in isolation.
		_, _ = utils.Kubectl("apply", "-f", "test/e2e/testdata/agentclass.yaml")
	})

	It("runs an agentReported AgentTask to Succeeded", func() {
		_, err := utils.Kubectl("apply", "-f", "test/e2e/testdata/agenttask.yaml")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) {
			return utils.ResourceField("agenttask", "e2e", "e2e-task", "{.status.phase}")
		}, "180s", "5s").Should(Equal("Succeeded"))
	})

	It("created the per-task completion mailbox ConfigMap and Role", func() {
		// agentReported tasks get a mailbox ConfigMap in agentry-system and a
		// per-task Role granting the gateway completion access.
		Eventually(func() error {
			_, err := utils.Kubectl("get", "configmap",
				"-n", "agentry-system", "-l", "agentry.io/task=e2e-task")
			return err
		}, "30s", "3s").Should(Succeed())
		out, err := utils.Kubectl("get", "role", "-n", "e2e", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("e2e-task"))
	})

	It("garbage-collects the task Pod after ttlSecondsAfterFinished", func() {
		Eventually(func() (string, error) {
			return utils.Kubectl("get", "pods", "-n", "e2e",
				"-l", "agentry.io/task=e2e-task", "--no-headers")
		}, "90s", "5s").Should(SatisfyAny(
			ContainSubstring("No resources found"),
			BeEmpty(),
		))
	})
})
```

- [ ] **Step 2: Verify the label selectors match the reconciler output**

The `agentry.io/task` label and the per-task Role name are assumptions. Before running, confirm them against the reconciler and adjust the selectors if they differ:

```bash
grep -rnE 'agentry.io/task|task.*Role|completion.*ConfigMap|Name:.*task' internal/controller/agenttask_desired.go | head
```
Expected: a label key for task pods (e.g. `agentry.io/task`) and the mailbox ConfigMap / Role naming. If the actual label is different (for example `agentry.io/agenttask`), update the `-l` selectors and the Role `ContainSubstring` in the spec to match.

- [ ] **Step 3: Run the task-lifecycle specs**

Run: `go test ./test/e2e/... -tags e2e -v -timeout 20m -args -ginkgo.focus="Task lifecycle"`
Expected: the task reaches Succeeded; the mailbox ConfigMap and per-task Role exist; the task Pod disappears within the TTL window.

- [ ] **Step 4: Run the full suite once**

Run: `go test ./test/e2e/... -tags e2e -v -timeout 20m`
Expected: Deployment, Golden path, and Task lifecycle all green in one run.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/task_lifecycle_test.go
git commit -m "Add task-lifecycle e2e specs (agentReported completion, mailbox, TTL GC)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: CI job

Add an `e2e` job to CI that spins up k3d and runs `make e2e`. Public repo, so Actions minutes are free.

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `make e2e` (Task 4).

- [ ] **Step 1: Add the e2e job**

Append this job to the `jobs:` map in `.github/workflows/ci.yml`:

```yaml
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Install k3d
        run: curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | TAG=v5.7.4 bash

      - name: Install Helm
        uses: azure/setup-helm@v4

      - name: Run e2e suite
        run: make e2e

      - name: Dump diagnostics on failure
        if: failure()
        run: |
          kubectl get all -A || true
          kubectl describe pods -n agentry-system || true
          kubectl describe pods -n e2e || true
          kubectl logs -n agentry-system -l app.kubernetes.io/component=controller --tail=200 || true
          kubectl logs -n agentry-system -l app.kubernetes.io/component=gateway --tail=200 || true
```

- [ ] **Step 2: Lint the workflow YAML**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))" && echo OK`
Expected: `OK` (valid YAML).

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "Add a k3d e2e job to CI

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

- [ ] **Step 4: (After push by the user) confirm the job is green**

The user pushes; watch the `e2e` job in the PR/branch CI. If it fails, the diagnostics step output pinpoints which pod or spec failed. Common first-run issues: k3d image import timing (retry), cert-manager webhook not yet ready (the k3d-up wait covers it), or the netpol ipset race on the probe (already wrapped in `Eventually`).

---

## Self-Review

**Spec coverage:**
- Substrate k3d, reuse `hack/k3d-up.sh` -> Task 4 (`make e2e` calls it).
- Real chart under test via `helm upgrade --install` -> Task 4 `e2e-deploy`.
- Hermetic (no LLM) -> dummy `e2e-openai-key`, echo agent, task posts to mailbox; no external calls.
- One image, task auto-complete hook -> Task 1 + `agenttask.yaml` env.
- Golden path specs 1-6 -> Task 6. Task lifecycle 7-8 -> Task 7.
- Local `make e2e` -> Task 4. CI job -> Task 8.
- Remove stale kbinit boilerplate -> Task 5 (suite), Task 4 (Makefile), Task 2 (utils).
- ipset race mitigation -> Task 6 netpol probe wrapped in `Eventually`; note in Task 8.

**Placeholder scan:** No TBD/TODO; all code blocks complete. The one runtime-derived assumption (task pod label / Role name) is explicitly verified in Task 7 Step 2 with a fallback instruction.

**Type consistency:** `parseForwardPort`, `PortForward`, `PostJSON`, `ResourceField`, `WaitRollout`, `Kubectl`, `Helm` defined in Task 2 and used with matching signatures in Tasks 5-7. `taskAutocompleteStatus` defined and used in Task 1. Image tags and namespaces match the Global Constraints throughout.
