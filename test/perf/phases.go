//go:build perftest

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

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The phases, which `perf run` executes in the order -phases gives. Each
// records its own block on the summary and cleans up its own objects, so a
// failed phase leaves the earlier numbers intact and the cluster reusable.

type gatewayResult struct {
	Legs []gatewayLeg `json:"legs"`
}

type gatewayLeg struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	MTLS        bool   `json:"mtls"`
	Stream      bool   `json:"stream,omitempty"`
	Format      string `json:"format,omitempty"`
	Concurrency int    `json:"concurrency"`
	// Client is what the loadgen saw. On a stream leg its latencyMs is time
	// to last byte and its ttfbMs time to first byte.
	Client            *loadResult `json:"client"`
	GatewaySideMs     stats       `json:"gatewaySideMs"`
	GatewayRequests   float64     `json:"gatewayRequests"`
	SpendUSD          float64     `json:"spendUsd"`
	BudgetUtilization float64     `json:"budgetUtilization"`
	// UsageMissing counts successful answers that carried no usage and so
	// settled at zero spend.
	UsageMissing float64 `json:"usageMissing,omitempty"`
	// GatewayCPUPerRequestMs is the gateway's process CPU over the leg,
	// summed across replicas, divided by the requests the gateway counted.
	GatewayCPUPerRequestMs float64 `json:"gatewayCpuPerRequestMs"`
	GatewayUsageMax        usage   `json:"gatewayUsageMax"`
}

type rampResult struct {
	Target           int          `json:"target"`
	WaveSize         int          `json:"waveSize"`
	Waves            []waveResult `json:"waves"`
	Achieved         int          `json:"achieved"`
	Saturation       string       `json:"saturation,omitempty"`
	SaturationWave   int          `json:"saturationWave,omitempty"`
	HostMemBeforeMiB float64      `json:"hostMemBeforeMiB"`
	MemPerAgentMiB   float64      `json:"memPerAgentMiB"`
}

type waveResult struct {
	Index           int                `json:"index"`
	Requested       int                `json:"requested"`
	ReadyInWave     int                `json:"readyInWave"`
	FleetReady      int                `json:"fleetReady"`
	WallSec         float64            `json:"wallSec"`
	TimeToReadySec  stats              `json:"timeToReadySec"`
	CertIssueSec    stats              `json:"certIssueSec"`
	PodStartSec     stats              `json:"podStartSec"`
	StartToReadySec stats              `json:"startToReadySec"`
	HostMemAvailMiB float64            `json:"hostMemAvailMiB"`
	Controller      usage              `json:"controller"`
	Gateway         usage              `json:"gateway"`
	ReconcileMs     stats              `json:"reconcileMs"`
	WorkqueueDepth  float64            `json:"workqueueDepth"`
	NodeMemMiB      map[string]float64 `json:"nodeMemMiB"`
}

type holdResult struct {
	Agents            int                `json:"agents"`
	Channels          int                `json:"channels"`
	ChannelsActiveSec float64            `json:"channelsActiveSec"`
	Client            *loadResult        `json:"client"`
	MessagesByStatus  map[string]float64 `json:"messagesByStatus"`
	MessageDurationMs stats              `json:"messageDurationMs"`
	DeliveryAttempts  map[string]float64 `json:"deliveryAttempts"`
	Audit             *apiAudit          `json:"audit,omitempty"`
	Series            []runtimeSample    `json:"series,omitempty"`
	Callbacks         float64            `json:"callbacks"`
	ReadyBefore       int                `json:"readyBefore"`
	ReadyAfter        int                `json:"readyAfter"`
	RestartsBefore    int32              `json:"restartsBefore"`
	RestartsAfter     int32              `json:"restartsAfter"`
	Flaps             int                `json:"flaps"`
	GatewayUsageMax   usage              `json:"gatewayUsageMax"`
	ControllerUsage   usage              `json:"controllerUsageMax"`
}

type teardownResult struct {
	Agents  int     `json:"agents"`
	Seconds float64 `json:"seconds"`
}

type churnResult struct {
	Agents              int                `json:"agents"`
	TimeToReadySec      stats              `json:"timeToReadySec"`
	AllReadySec         float64            `json:"allReadySec"`
	FirstHibernationSec stats              `json:"firstHibernationSec"`
	AllHibernatedSec    float64            `json:"allHibernatedSec"`
	Client              *loadResult        `json:"client"`
	MessagesByStatus    map[string]float64 `json:"messagesByStatus"`
	WakeDurationMs      stats              `json:"wakeDurationMs"`
	WakesByResult       map[string]float64 `json:"wakesByResult"`
	WakesTotal          float64            `json:"wakesTotal"`
	HibernationsTotal   float64            `json:"hibernationsTotal"`
	Callbacks           float64            `json:"callbacks"`
	TeardownSec         float64            `json:"teardownSec"`
	Idle                *apiAudit          `json:"idle,omitempty"`
}

// apiAudit is what the operator asked of the control plane over a window:
// client-side requests by method per component (rest_client_requests_total),
// apiserver requests by verb and resource across every client, and the
// per-agent-minute write rates, which transfer to any cluster.
type apiAudit struct {
	Seconds                        float64            `json:"seconds"`
	Agents                         int                `json:"agents"`
	Controller                     map[string]float64 `json:"controller"`
	Gateway                        map[string]float64 `json:"gateway"`
	APIServer                      map[string]float64 `json:"apiserver"`
	ControllerRequestsPerSec       float64            `json:"controllerRequestsPerSec"`
	GatewayRequestsPerSec          float64            `json:"gatewayRequestsPerSec"`
	ControllerWritesPerAgentMinute float64            `json:"controllerWritesPerAgentMinute"`
	GatewayWritesPerAgentMinute    float64            `json:"gatewayWritesPerAgentMinute"`
}

// runtimeSample is one reading of a component's Go runtime during a hold.
type runtimeSample struct {
	AtSec      float64 `json:"atSec"`
	Component  string  `json:"component"`
	Goroutines float64 `json:"goroutines"`
	HeapMiB    float64 `json:"heapMiB"`
	RSSMiB     float64 `json:"rssMiB"`
}

// restartResult is what a rolling restart costs with the fleet up: the
// controller's rollout and its time to the first reconcile afterwards
// (leader handoff included), and the gateway's rollout under LLM traffic
// with the requests that failed while it rolled.
type restartResult struct {
	Agents                      int         `json:"agents"`
	ControllerRolloutSec        float64     `json:"controllerRolloutSec"`
	ControllerFirstReconcileSec float64     `json:"controllerFirstReconcileSec"`
	GatewayRolloutSec           float64     `json:"gatewayRolloutSec"`
	GatewayClient               *loadResult `json:"gatewayClient"`
	GatewayFailed               int         `json:"gatewayFailed"`
}

// auditSnapshots is the three scrapes an audit window starts and ends with.
type auditSnapshots struct {
	at           time.Time
	gw, ctl, api *snapshot
}

func (h *harness) auditSnapshot(ctx context.Context) (*auditSnapshots, error) {
	gw, err := h.scrapeGateway(ctx)
	if err != nil {
		return nil, err
	}
	ctl, err := h.scrapeController(ctx)
	if err != nil {
		return nil, err
	}
	api, err := h.k.scrapeAPIServer(ctx)
	if err != nil {
		return nil, err
	}
	return &auditSnapshots{at: time.Now(), gw: gw, ctl: ctl, api: api}, nil
}

func writeRequests(byMethod map[string]float64) float64 {
	return byMethod["POST"] + byMethod["PUT"] + byMethod["PATCH"] + byMethod["DELETE"]
}

// audit folds two snapshot sets into the per-window numbers.
func audit(before, after *auditSnapshots, agents int) *apiAudit {
	secs := after.at.Sub(before.at).Seconds()
	a := &apiAudit{
		Seconds:    round3(secs),
		Agents:     agents,
		Controller: counterByLabel(before.ctl, after.ctl, "rest_client_requests_total", "method", nil),
		Gateway:    counterByLabel(before.gw, after.gw, "rest_client_requests_total", "method", nil),
		APIServer:  apiRequestsByVerbResource(before.api, after.api),
	}
	var ctlTotal, gwTotal float64
	for _, v := range a.Controller {
		ctlTotal += v
	}
	for _, v := range a.Gateway {
		gwTotal += v
	}
	if secs > 0 {
		a.ControllerRequestsPerSec = round3(ctlTotal / secs)
		a.GatewayRequestsPerSec = round3(gwTotal / secs)
		if agents > 0 {
			a.ControllerWritesPerAgentMinute = round3(writeRequests(a.Controller) / float64(agents) / (secs / 60))
			a.GatewayWritesPerAgentMinute = round3(writeRequests(a.Gateway) / float64(agents) / (secs / 60))
		}
	}
	return a
}

func (h *harness) logAudit(label string, a *apiAudit) {
	h.logf("  %s over %.0fs, %d agents: controller %.1f req/s (%v), %.2f writes/agent/min; "+
		"gateway %.1f req/s (%v), %.2f writes/agent/min",
		label, a.Seconds, a.Agents, a.ControllerRequestsPerSec, topEntries(a.Controller, 4),
		a.ControllerWritesPerAgentMinute, a.GatewayRequestsPerSec, topEntries(a.Gateway, 4),
		a.GatewayWritesPerAgentMinute)
	h.logf("  %s apiserver by verb and resource: %v", label, topEntries(a.APIServer, 10))
}

// runtimeSampler reads goroutines, heap in use, and RSS from both
// components on an interval; over a long hold the series is the soak.
type runtimeSampler struct {
	mu      sync.Mutex
	samples []runtimeSample
	stop    chan struct{}
	done    chan struct{}
}

func (h *harness) sampleRuntime(ctx context.Context, interval time.Duration) *runtimeSampler {
	s := &runtimeSampler{stop: make(chan struct{}), done: make(chan struct{})}
	start := time.Now()
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(interval):
			}
			for comp, scrape := range map[string]func(context.Context) (*snapshot, error){
				"gateway": h.scrapeGateway, "controller": h.scrapeController,
			} {
				snap, err := scrape(ctx)
				if err != nil {
					continue
				}
				smp := runtimeSample{
					AtSec: round3(time.Since(start).Seconds()), Component: comp,
					Goroutines: snap.gauge("go_goroutines", nil),
					HeapMiB:    round3(snap.gauge("go_memstats_heap_inuse_bytes", nil) / (1 << 20)),
					RSSMiB:     round3(snap.gauge("process_resident_memory_bytes", nil) / (1 << 20)),
				}
				s.mu.Lock()
				s.samples = append(s.samples, smp)
				s.mu.Unlock()
				h.logf("    %s at %.0fs: %.0f goroutines, heap %.1f MiB, rss %.1f MiB",
					comp, smp.AtSec, smp.Goroutines, smp.HeapMiB, smp.RSSMiB)
			}
		}
	}()
	return s
}

func (s *runtimeSampler) finish() []runtimeSample {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples
}

type tasksResult struct {
	Submitted        int            `json:"submitted"`
	SubmitSec        float64        `json:"submitSec"`
	Phases           map[string]int `json:"phases"`
	ProvisionSec     stats          `json:"provisionSec"`
	RunSec           stats          `json:"runSec"`
	TotalSec         stats          `json:"totalSec"`
	MakespanSec      float64        `json:"makespanSec"`
	ThroughputPerMin float64        `json:"throughputPerMin"`
	Retries          int32          `json:"retries"`
	TeardownSec      float64        `json:"teardownSec"`
}

func (h *harness) scrapeGateway(ctx context.Context) (*snapshot, error) {
	return h.k.scrapeComponent(ctx, "kaalm-system", "gateway", "9090")
}

func (h *harness) scrapeController(ctx context.Context) (*snapshot, error) {
	return h.k.scrapeComponent(ctx, "kaalm-system", "controller", "8080")
}

// usageSampler records the peak metrics-API usage per chart component while
// a load job runs.
type usageSampler struct {
	mu   sync.Mutex
	peak map[string]usage
	stop chan struct{}
	done chan struct{}
}

func (h *harness) sampleUsage(ctx context.Context) *usageSampler {
	s := &usageSampler{peak: map[string]usage{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			if u, err := h.k.podUsage(ctx, "kaalm-system"); err == nil {
				s.mu.Lock()
				for name, cur := range u {
					p := s.peak[name]
					p.CPUMilli = math.Max(p.CPUMilli, cur.CPUMilli)
					p.MemMiB = math.Max(p.MemMiB, cur.MemMiB)
					s.peak[name] = p
				}
				s.mu.Unlock()
			}
			select {
			case <-s.stop:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return s
}

func (s *usageSampler) finish() map[string]usage {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]usage{}
	for k, v := range s.peak {
		out[k] = usage{CPUMilli: round3(v.CPUMilli), MemMiB: round3(v.MemMiB)}
	}
	return out
}

// ---- gateway ----

var gatewayProviders = []string{providerFast, providerSlow, providerHard, providerLimited}

// Gateway-side metric names the legs read.
const (
	metricLLMDuration  = "kaalm_llm_request_duration_seconds"
	metricLLMRequests  = "kaalm_llm_requests_total"
	metricToolDuration = "kaalm_tool_call_duration_seconds"
	metricToolCalls    = "kaalm_tool_calls_total"
	metricProcessCPU   = "process_cpu_seconds_total"
	// metricReconcileTime is controller-runtime's per-controller reconcile
	// histogram, labeled by labelController.
	metricReconcileTime = "controller_runtime_reconcile_time_seconds"
	labelController     = "controller"
)

// gatewayLegDef is one leg of the gateway table: a provider reached over one
// auth mode.
type gatewayLegDef struct {
	name, provider string
	mtls           bool
}

// gatewayLegs is the gateway phase's table. The token tier is the
// gateway-only adoption path (a ServiceAccount token validated once and
// cached); the mTLS legs are the primary path every Kaalm-managed agent
// takes, presented here through a borrowed agent identity. Upstream latency
// and budget mode vary on that path. The providers phase reruns the first
// two so its rows compare with these.
var gatewayLegs = []gatewayLegDef{
	{"token tier: soft budget, 0 ms upstream", providerFast, false},
	{"mtls: soft budget, 0 ms upstream", providerFast, true},
	{"mtls: soft budget, 50 ms upstream", providerSlow, true},
	{"mtls: hard budget, 0 ms upstream", providerHard, true},
	{"mtls: rate limits on, 0 ms upstream", providerLimited, true},
}

// legSpec is one fixed-concurrency loadgen run against the gateway and the
// gateway-side series that describe it.
type legSpec struct {
	name     string
	provider string
	job      string
	mtls     bool
	mode     string
	// args are the loadgen flags beyond the mode, concurrency, duration,
	// and client certificate, which runLeg adds.
	args []string
	// histogram and counter are the gateway-side duration histogram and
	// request counter for this leg's traffic, both filtered by labels.
	histogram, counter string
	labels             map[string]string
	// stream and format only label the result; the loadgen flags in args
	// select them.
	stream bool
	format string
}

// llmLegSpec is a chat leg against one provider: the gateway table's shape.
func llmLegSpec(name, provider, job string, mtls bool, args ...string) legSpec {
	return legSpec{
		name: name, provider: provider, job: job, mtls: mtls, mode: modeGateway,
		args:      append([]string{"-model", provider + "/mock-model"}, args...),
		histogram: metricLLMDuration, counter: metricLLMRequests,
		labels: map[string]string{"provider": provider},
	}
}

// gatewayLegSpec turns a table row into its leg, with the job names the
// gateway phase has always used.
func gatewayLegSpec(d gatewayLegDef) legSpec {
	job := "loadgen-gateway-token-" + d.provider
	if d.mtls {
		job = "loadgen-gateway-mtls-" + d.provider
	}
	return llmLegSpec(d.name, d.provider, job, d.mtls)
}

// legScrapes are the gateway snapshots a leg starts and ends with, for
// figures beyond the common ones.
type legScrapes struct {
	before, after *snapshot
}

// runLeg runs one leg: a gateway scrape, the loadgen Job with the usage
// sampler running, and a second scrape. The common figures land on the
// returned gatewayLeg; LLM legs also get spend, budget, and missing usage.
func (h *harness) runLeg(ctx context.Context, spec legSpec, mtlsSecret string) (gatewayLeg, legScrapes, error) {
	before, err := h.scrapeGateway(ctx)
	if err != nil {
		return gatewayLeg{}, legScrapes{}, err
	}
	args := append([]string{flagMode, spec.mode}, spec.args...)
	args = append(args,
		"-concurrency", strconv.Itoa(h.cfg.GatewayConcurrency),
		flagDuration, h.cfg.GatewayDuration.String(),
	)
	secret := ""
	if spec.mtls {
		args = append(args, "-cert-file", "/var/run/mtls/tls.crt", "-key-file", "/var/run/mtls/tls.key")
		secret = mtlsSecret
	}
	sampler := h.sampleUsage(ctx)
	client, err := h.k.runLoadgen(ctx, h.cfg.Namespace, spec.job, h.cfg.LoadgenImage, args, secret,
		h.cfg.GatewayDuration+3*time.Minute)
	peak := sampler.finish()
	if err != nil {
		return gatewayLeg{}, legScrapes{}, fmt.Errorf("leg %s: %w", spec.name, err)
	}
	after, err := h.scrapeGateway(ctx)
	if err != nil {
		return gatewayLeg{}, legScrapes{}, err
	}
	leg := gatewayLeg{
		Name:            spec.name,
		Provider:        spec.provider,
		MTLS:            spec.mtls,
		Stream:          spec.stream,
		Format:          spec.format,
		Concurrency:     h.cfg.GatewayConcurrency,
		Client:          client,
		GatewaySideMs:   histStats(histogramDelta(before, after, spec.histogram, spec.labels)),
		GatewayRequests: counterDelta(before, after, spec.counter, spec.labels),
		GatewayUsageMax: peak["kaalm-gateway"],
	}
	leg.GatewayCPUPerRequestMs = cpuPerRequestMs(counterDelta(before, after, metricProcessCPU, nil), leg.GatewayRequests)
	if spec.counter == metricLLMRequests {
		leg.SpendUSD = round3(counterDelta(before, after, "kaalm_llm_spend_usd_total", spec.labels))
		leg.BudgetUtilization = after.gauge("kaalm_llm_budget_utilization", spec.labels)
		leg.UsageMissing = counterDelta(before, after, "kaalm_llm_usage_missing_total", spec.labels)
	}
	h.logf("  client: %d requests, %.1f rps, p50 %.1f ms, p99 %.1f ms, statuses %v; gateway %.3f ms CPU per request",
		client.Requests, client.RPS, client.LatencyMs.P50, client.LatencyMs.P99, client.Statuses,
		leg.GatewayCPUPerRequestMs)
	return leg, legScrapes{before: before, after: after}, nil
}

// loadgenIdentity creates an agent whose identity the mTLS legs borrow and
// waits for its TLS Secret: the controller issues the certificate (SAN
// {name}.{ns}.svc.cluster.local) and the loadgen Job presents it, so the
// gateway sees a Kaalm-managed agent calling from its own namespace. The
// cleanup deletes the agent.
func (h *harness) loadgenIdentity(ctx context.Context, agent *kaalmv1beta1.Agent) (string, func(), error) {
	if err := h.k.createAll(ctx, []client.Object{agent}); err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = h.k.c.Delete(context.Background(), agent) }
	// The agent's TLS Secret is the one its Certificate names in spec.secretName.
	var mtlsSecret string
	if err := pollUntil(ctx, 3*time.Minute, 2*time.Second, func() (bool, error) {
		var cert cmapi.Certificate
		key := client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name + "-tls"}
		if err := h.k.c.Get(ctx, key, &cert); err != nil || cert.Spec.SecretName == "" {
			return false, nil //nolint:nilerr // absent until the controller creates it
		}
		mtlsSecret = cert.Spec.SecretName
		sec, err := h.k.cs.CoreV1().Secrets(agent.Namespace).Get(ctx, mtlsSecret, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr // absent until cert-manager issues it
		}
		return len(sec.Data["tls.crt"]) > 0 && len(sec.Data["tls.key"]) > 0, nil
	}); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("waiting for the %s agent certificate: %w", agent.Name, err)
	}
	return mtlsSecret, cleanup, nil
}

func (h *harness) runGateway(ctx context.Context) error {
	if err := h.k.ensureClass(ctx, activeClass()); err != nil {
		return err
	}
	agent := loadgenAgentObj(h.cfg.Namespace, activeClassName, h.cfg.AgentImage, gatewayProviders)
	mtlsSecret, cleanup, err := h.loadgenIdentity(ctx, agent)
	if err != nil {
		return err
	}
	defer cleanup()

	res := &gatewayResult{}
	for i, d := range gatewayLegs {
		h.logf("gateway leg %d/%d: %s (%d callers, %s)",
			i+1, len(gatewayLegs), d.name, h.cfg.GatewayConcurrency, h.cfg.GatewayDuration)
		leg, _, err := h.runLeg(ctx, gatewayLegSpec(d), mtlsSecret)
		if err != nil {
			return fmt.Errorf("gateway %w", err)
		}
		res.Legs = append(res.Legs, leg)
	}
	h.sum.Gateway = res
	return nil
}

// ---- tools ----

const (
	toolsClassName = "perf-tools"
	// toolsAgentName is the Agent whose identity the mTLS tool legs
	// borrow; it holds a grant on each ToolProvider, narrowed to the called
	// tool.
	toolsAgentName = "loadgen-tools"
)

// toolsResult is the tool plane under load: brokered tools/call through
// POST /v1/mcp/perf-mcp by auth mode, and through /v1/mcp/perf-mcp-legacy
// in the legacy session era.
type toolsResult struct {
	Legs []toolLeg `json:"legs"`
}

type toolLeg struct {
	Name        string      `json:"name"`
	Provider    string      `json:"provider"`
	MTLS        bool        `json:"mtls"`
	Concurrency int         `json:"concurrency"`
	Client      *loadResult `json:"client"`
	// GatewaySideMs is the broker's histogram, which counts forwarded
	// calls only.
	GatewaySideMs          stats              `json:"gatewaySideMs"`
	CallsByStatus          map[string]float64 `json:"callsByStatus"`
	GatewayCalls           float64            `json:"gatewayCalls"`
	GatewayCPUPerRequestMs float64            `json:"gatewayCpuPerRequestMs"`
	GatewayUsageMax        usage              `json:"gatewayUsageMax"`
}

// toolsClass is the active class with both perf ToolProviders allowed,
// under its own name so the ramp's class never changes.
func toolsClass() *kaalmv1beta1.AgentClass {
	c := activeClass()
	c.Name = toolsClassName
	c.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{
		{Name: toolProviderName}, {Name: toolLegacyProviderName},
	}
	return c
}

func toolsAgentObj(ns, image string) *kaalmv1beta1.Agent {
	a := agentObj(ns, toolsAgentName, toolsClassName, image, phaseTools, false)
	a.Spec.Tools = []kaalmv1beta1.AgentToolGrant{
		{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: toolProviderName}, Tools: []string{toolName}},
		{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: toolLegacyProviderName}, Tools: []string{toolName}},
	}
	return a
}

// toolLegSpecs are the tools phase's legs. The token-tier caller has no
// workload, so the broker admits it on the provider's allowedNamespaces and
// declared catalog; the mTLS callers go through the agent's grants and the
// class allowlist. The legacy session leg is mTLS only: legacy-era clients
// are agent frameworks in Kaalm-managed agents, the broker binds the
// session to the workload identity, and the token-versus-mTLS cost is
// already in the first two legs.
var toolLegSpecs = []legSpec{
	toolLegSpec("tools token tier: 2026-07-28, immediate tool server", "loadgen-tools-token",
		toolProviderName, false),
	toolLegSpec("tools mtls: 2026-07-28, immediate tool server", "loadgen-tools-mtls",
		toolProviderName, true),
	toolLegSpec("tools mtls: legacy session, immediate tool server", "loadgen-tools-legacy-mtls",
		toolLegacyProviderName, true,
		"-tool-url", gatewayBase+"/v1/mcp/"+toolLegacyProviderName, "-legacy-session"),
}

// toolLegSpec reads the broker's figures for tools/call of toolName only,
// so a legacy caller's initialize and notification, which name no tool,
// stay out of the latency and the call counts. Denials the broker makes
// before it reads the body (namespace, grant, class, rate limit) carry no
// tool label either, so on every tool leg they show only in the client's
// statuses. The gateway figures also include the warmup's successful call,
// which the client's recorder starts after.
func toolLegSpec(name, job, provider string, mtls bool, args ...string) legSpec {
	return legSpec{
		name: name, provider: provider, job: job, mtls: mtls, mode: modeTools, args: args,
		histogram: metricToolDuration, counter: metricToolCalls,
		labels: map[string]string{"provider": provider, "tool": toolName},
	}
}

func (h *harness) runTools(ctx context.Context) error {
	cfg := h.cfg
	// Checked here rather than with the rest of the infrastructure, so a
	// cluster without the image fails only this phase.
	for _, deploy := range []string{"mock-mcp", "mock-mcp-legacy"} {
		if err := h.k.kubectl("rollout", "status", "deploy/"+deploy, "-n", cfg.Namespace, "--timeout=180s"); err != nil {
			return fmt.Errorf("mock MCP server %s not ready (make perf-images builds and imports it): %w", deploy, err)
		}
	}
	if err := h.k.ensureClass(ctx, toolsClass()); err != nil {
		return err
	}
	mtlsSecret, cleanup, err := h.loadgenIdentity(ctx, toolsAgentObj(cfg.Namespace, cfg.AgentImage))
	if err != nil {
		return err
	}
	defer cleanup()

	res := &toolsResult{}
	for i, spec := range toolLegSpecs {
		h.logf("tools leg %d/%d: %s (%d callers, %s)",
			i+1, len(toolLegSpecs), spec.name, cfg.GatewayConcurrency, cfg.GatewayDuration)
		leg, scrapes, err := h.runLeg(ctx, spec, mtlsSecret)
		if err != nil {
			return fmt.Errorf("tools %w", err)
		}
		res.Legs = append(res.Legs, toolLeg{
			Name:                   leg.Name,
			Provider:               leg.Provider,
			MTLS:                   leg.MTLS,
			Concurrency:            leg.Concurrency,
			Client:                 leg.Client,
			GatewaySideMs:          leg.GatewaySideMs,
			CallsByStatus:          counterByLabel(scrapes.before, scrapes.after, metricToolCalls, "status", spec.labels),
			GatewayCalls:           leg.GatewayRequests,
			GatewayCPUPerRequestMs: leg.GatewayCPUPerRequestMs,
			GatewayUsageMax:        leg.GatewayUsageMax,
		})
	}
	h.sum.Tools = res
	return nil
}

// ---- stream ----

// streamResult is the SSE relay under load, token tier only: auth costs the
// same per request on either tier (the gateway table shows it), and
// streaming changes only the relay.
type streamResult struct {
	Legs []gatewayLeg `json:"legs"`
}

var streamLegSpecs = []legSpec{
	streamLegSpec(llmLegSpec("stream openai: token tier, immediate upstream", providerFast,
		"loadgen-stream-fast", false, "-stream"), formatOpenAI),
	streamLegSpec(llmLegSpec("stream anthropic: token tier, immediate upstream", providerAnthropic,
		"loadgen-stream-anthropic", false, "-stream", "-format", formatAnthropic, "-url", gatewayBase+"/v1/messages"),
		formatAnthropic),
	streamLegSpec(llmLegSpec("stream openai: token tier, 10 ms between events", providerPaced,
		"loadgen-stream-paced", false, "-stream"), formatOpenAI),
}

// streamLegSpec marks a leg as streamed in the given format; runLeg copies
// both onto the result.
func streamLegSpec(spec legSpec, format string) legSpec {
	spec.stream, spec.format = true, format
	return spec
}

func (h *harness) runStream(ctx context.Context) error {
	res := &streamResult{}
	for i, spec := range streamLegSpecs {
		h.logf("stream leg %d/%d: %s (%d callers, %s)",
			i+1, len(streamLegSpecs), spec.name, h.cfg.GatewayConcurrency, h.cfg.GatewayDuration)
		leg, _, err := h.runLeg(ctx, spec, "")
		if err != nil {
			return fmt.Errorf("stream %w", err)
		}
		if ttfb := leg.Client.TTFBMs; ttfb != nil {
			h.logf("  time to first byte p50 %.1f ms p99 %.1f ms; spend %.3f USD, %.0f answers without usage",
				ttfb.P50, ttfb.P99, leg.SpendUSD, leg.UsageMissing)
		}
		res.Legs = append(res.Legs, leg)
	}
	h.sum.Stream = res
	return nil
}

// ---- ramp ----

// rampFleet is the standard max-active fleet: ramp-NNNN in the run's
// namespace, which the hold, restart, and teardown phases reuse.
func (h *harness) rampFleet() fleet {
	return fleet{
		phase: phaseRamp, prefix: "ramp-", target: h.cfg.RampTarget,
		namespaces: []string{h.cfg.Namespace}, holdJob: "loadgen-hold",
	}
}

// saturation names the first environmental limit the ramp has hit, or "".
// Pending and crash-looping pods are counted in scope (one namespace, or
// every namespace when scope is "").
func (h *harness) saturation(ctx context.Context, scope string) (string, error) {
	mem, err := hostMemAvailableMiB()
	if err != nil {
		return "", err
	}
	if mem < float64(h.cfg.MemFloorMiB) {
		return fmt.Sprintf("host MemAvailable %.0f MiB is below the %d MiB floor", mem, h.cfg.MemFloorMiB), nil
	}
	var nodes corev1.NodeList
	if err := h.k.c.List(ctx, &nodes); err != nil {
		return "", err
	}
	for i := range nodes.Items {
		for _, c := range nodes.Items[i].Status.Conditions {
			if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
				return "node " + nodes.Items[i].Name + " reports MemoryPressure", nil
			}
		}
	}
	var pods corev1.PodList
	if err := h.k.c.List(ctx, &pods, client.InNamespace(scope)); err != nil {
		return "", err
	}
	// Agent pods that crash-loop are the environment giving out from the
	// inside: probes time out before any node reports pressure or the
	// scheduler refuses anything. The first baseline run saw exactly that,
	// with 3.9 GiB of "available" host memory and 422 MiB actually free.
	crashLooping := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels["kaalm.io/workload"] == workloadAgent {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
					crashLooping++
				}
			}
		}
		if p.Status.Phase != corev1.PodPending {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				return "scheduler: " + c.Message, nil
			}
		}
	}
	if crashLooping > 0 {
		return fmt.Sprintf("agent pods failing probes: %d in CrashLoopBackOff", crashLooping), nil
	}
	return "", nil
}

func (h *harness) runRamp(ctx context.Context) error {
	res, err := h.ramp(ctx, h.rampFleet())
	if err != nil {
		return err
	}
	h.sum.Ramp = res
	return nil
}

// ramp creates a fleet in waves of agents that are never retired, until it
// reaches its target or the environment saturates; the wave that saturates
// is trimmed.
func (h *harness) ramp(ctx context.Context, f fleet) (*rampResult, error) {
	cfg := h.cfg
	if err := h.k.ensureClass(ctx, activeClass()); err != nil {
		return nil, err
	}
	memBefore, err := hostMemAvailableMiB()
	if err != nil {
		return nil, err
	}
	res := &rampResult{Target: f.target, WaveSize: cfg.WaveSize, HostMemBeforeMiB: round3(memBefore)}
	prevCtl, err := h.scrapeController(ctx)
	if err != nil {
		return nil, err
	}
	for wave := 0; wave*cfg.WaveSize < f.target; wave++ {
		lo, hi := wave*cfg.WaveSize, (wave+1)*cfg.WaveSize
		if hi > f.target {
			hi = f.target
		}
		names := map[string]string{} // agent name to namespace
		objs := make([]client.Object, 0, 2*(hi-lo))
		for i := lo; i < hi; i++ {
			name, ns := f.name(i), f.namespaceOf(i)
			names[name] = ns
			// The channel is created with its agent so the channel
			// reconciler's work overlaps the waves instead of stacking up
			// in front of the hold phase.
			objs = append(objs,
				agentObj(ns, name, activeClassName, cfg.AgentImage, f.phase, false),
				channelObj(ns, name, f.phase, h.callbackURL()))
		}
		h.logf("%s wave %d: creating agents %d..%d with their channels", f.phase, wave, lo, hi-1)
		waveStart := time.Now()
		if err := h.k.createAll(ctx, objs); err != nil {
			return nil, err
		}
		var sat string
		var timings []agentTiming
		err := pollUntil(ctx, cfg.WaveTimeout, 3*time.Second, func() (bool, error) {
			ts, err := h.k.agentTimings(ctx, f.scope(), f.phase)
			if err != nil {
				return false, err
			}
			timings = ts
			if readyAmong(ts, names) == len(names) {
				return true, nil
			}
			s, err := h.saturation(ctx, f.scope())
			if err != nil {
				return false, err
			}
			if s != "" {
				sat = s
				return true, nil
			}
			return false, nil
		})
		if errors.Is(err, errTimeout) {
			sat = fmt.Sprintf("wave %d: %d of %d agents Ready within %s",
				wave, readyAmong(timings, names), len(names), cfg.WaveTimeout)
		} else if err != nil {
			return nil, err
		}
		wall := time.Since(waveStart)

		inWave := filterTimings(timings, names)
		w := waveResult{
			Index:           wave,
			Requested:       len(names),
			ReadyInWave:     countReady(inWave),
			FleetReady:      countReady(timings),
			WallSec:         round3(wall.Seconds()),
			TimeToReadySec:  summarize(secondsBetween(inWave, created, readyAt)),
			CertIssueSec:    summarize(secondsBetween(inWave, created, certReadyAt)),
			PodStartSec:     summarize(secondsBetween(inWave, created, podStartedAt)),
			StartToReadySec: summarize(secondsBetween(inWave, podStartedAt, readyAt)),
			NodeMemMiB:      map[string]float64{},
		}
		if mem, err := hostMemAvailableMiB(); err == nil {
			w.HostMemAvailMiB = round3(mem)
		}
		if u, err := h.k.podUsage(ctx, "kaalm-system"); err == nil {
			w.Controller, w.Gateway = u["kaalm-controller"], u["kaalm-gateway"]
		}
		if nu, err := h.k.nodeUsage(ctx); err == nil {
			for name, u := range nu {
				w.NodeMemMiB[name] = round3(u.MemMiB)
			}
		}
		if ctl, err := h.scrapeController(ctx); err == nil {
			agentReconciles := map[string]string{labelController: agentControllerName}
			w.ReconcileMs = histStats(histogramDelta(prevCtl, ctl, metricReconcileTime, agentReconciles))
			w.WorkqueueDepth = ctl.gauge("workqueue_depth", map[string]string{"name": "agent"})
			prevCtl = ctl
		}
		res.Waves = append(res.Waves, w)
		res.Achieved = w.FleetReady
		h.logf("  wave %d: %d/%d Ready in %.0fs (fleet %d Ready), time-to-Ready p50 %.0fs p95 %.0fs, host avail %.0f MiB",
			wave, w.ReadyInWave, w.Requested, w.WallSec, w.FleetReady,
			w.TimeToReadySec.P50, w.TimeToReadySec.P95, w.HostMemAvailMiB)
		if sat != "" {
			res.Saturation = sat
			res.SaturationWave = wave
			h.logf("  %s stops: %s", f.phase, sat)
			// The wave that hit the ceiling is trimmed, so the fleet the
			// later phases run on is the largest one that came up clean.
			h.logf("  trimming wave %d (%d agents)", wave, len(names))
			if err := h.k.deleteAgents(ctx, names, 5*time.Minute); err != nil {
				h.note("trimming wave %d: %v", wave, err)
			}
			if ts, err := h.k.agentTimings(ctx, f.scope(), f.phase); err == nil {
				res.Achieved = countReady(ts)
			}
			break
		}
	}
	if res.Achieved > 0 {
		if memNow, err := hostMemAvailableMiB(); err == nil {
			res.MemPerAgentMiB = round3((memBefore - memNow) / float64(res.Achieved))
		}
	}
	return res, nil
}

func readyAmong(ts []agentTiming, names map[string]string) int {
	n := 0
	for _, t := range ts {
		if ns, ok := names[t.Name]; ok && ns == t.Namespace && t.ReadyNow {
			n++
		}
	}
	return n
}

func filterTimings(ts []agentTiming, names map[string]string) []agentTiming {
	var out []agentTiming
	for _, t := range ts {
		if ns, ok := names[t.Name]; ok && ns == t.Namespace {
			out = append(out, t)
		}
	}
	return out
}

// ---- hold ----

func (h *harness) runHold(ctx context.Context) error {
	res, err := h.hold(ctx, h.rampFleet())
	if err != nil {
		return err
	}
	h.sum.Hold = res
	return nil
}

// holdLoadgenArgs is the hold's channel traffic: one message per agent per
// interval, round-robin over the first count agents. A fleet spread over
// several namespaces puts {ns} in the path prefix and passes the namespace
// list, so the loadgen places agent i where the fleet did.
func holdLoadgenArgs(f fleet, count int, rate float64, duration time.Duration) []string {
	prefix := "/channels/" + f.namespaces[0] + "/" + f.prefix
	var spread []string
	if len(f.namespaces) > 1 {
		prefix = "/channels/{ns}/" + f.prefix
		spread = []string{"-namespaces", strings.Join(f.namespaces, ",")}
	}
	args := []string{
		flagMode, modeChannels,
		"-path-prefix", prefix,
		"-count", strconv.Itoa(count),
		"-pad", "4",
		"-rate", strconv.FormatFloat(rate, 'f', 4, 64),
		flagDuration, duration.String(),
	}
	return append(args, spread...)
}

// hold drives messages across a fleet's Ready agents for the hold duration
// and reads what the gateway, the agents, and the control plane did.
func (h *harness) hold(ctx context.Context, f fleet) (*holdResult, error) {
	cfg := h.cfg
	timings, err := h.k.agentTimings(ctx, f.scope(), f.phase)
	if err != nil {
		return nil, err
	}
	if len(timings) == 0 {
		return nil, fmt.Errorf("hold needs the %[1]s fleet; run the %[1]s phase first", f.phase)
	}
	// Messages go to the longest run of Ready agents from index 0, so a
	// partially failed final wave never turns delivery errors into noise.
	readySet := map[string]bool{}
	for _, t := range timings {
		if t.ReadyNow {
			readySet[t.Name] = true
		}
	}
	count := 0
	for readySet[f.name(count)] {
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("hold: no Ready %s agents", f.phase)
	}

	objs := make([]client.Object, 0, len(timings))
	for _, t := range timings {
		objs = append(objs, channelObj(t.Namespace, t.Name, f.phase, h.callbackURL()))
	}
	h.logf("hold: creating %d channels", len(objs))
	chStart := time.Now()
	if err := h.k.createAll(ctx, objs); err != nil {
		return nil, err
	}
	var active, total int
	if err := pollUntil(ctx, 10*time.Minute, 3*time.Second, func() (bool, error) {
		var err error
		active, total, err = h.k.channelsActive(ctx, f.scope(), f.phase)
		return active == total, err
	}); err != nil && !errors.Is(err, errTimeout) {
		return nil, err
	}
	res := &holdResult{Agents: len(timings), Channels: total, ChannelsActiveSec: round3(time.Since(chStart).Seconds())}
	h.logf("  %d/%d channels active after %.0fs", active, total, res.ChannelsActiveSec)

	res.ReadyBefore = countReady(timings)
	res.RestartsBefore = sumRestarts(timings)
	auditBefore, err := h.auditSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	before := auditBefore.gw
	rate := float64(count) / cfg.HoldPerAgentInterval.Seconds()
	h.logf("hold: %d agents, %.2f msg/s for %s (one message per agent per %s)",
		count, rate, cfg.HoldDuration, cfg.HoldPerAgentInterval)
	holdStart := time.Now()
	sampler := h.sampleUsage(ctx)
	runtimeSamples := h.sampleRuntime(ctx, cfg.HoldSampleInterval)
	client, err := h.k.runLoadgen(ctx, cfg.Namespace, f.holdJob, cfg.LoadgenImage,
		holdLoadgenArgs(f, count, rate, cfg.HoldDuration), "", cfg.HoldDuration+3*time.Minute)
	peak := sampler.finish()
	res.Series = runtimeSamples.finish()
	if err != nil {
		return nil, err
	}
	res.Client = client
	// Let detached deliveries and callbacks settle before reading the counters.
	if err := sleepCtx(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	auditAfter, err := h.auditSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	after := auditAfter.gw
	res.Audit = audit(auditBefore, auditAfter, count)
	h.logAudit("hold audit", res.Audit)
	webhook := map[string]string{"channel_type": channelTypeWebhook}
	res.MessagesByStatus = counterByLabel(before, after, "kaalm_channel_messages_total", "status", webhook)
	res.MessageDurationMs = histStats(histogramDelta(before, after, "kaalm_channel_message_duration_seconds", webhook))
	res.DeliveryAttempts = counterByLabel(before, after, "kaalm_channel_delivery_attempts_total", "outcome", nil)
	res.Callbacks = counterDelta(before, after, "kaalm_channel_callback_total", nil)
	res.GatewayUsageMax = peak["kaalm-gateway"]
	res.ControllerUsage = peak["kaalm-controller"]

	afterTimings, err := h.k.agentTimings(ctx, f.scope(), f.phase)
	if err != nil {
		return nil, err
	}
	res.ReadyAfter = countReady(afterTimings)
	res.RestartsAfter = sumRestarts(afterTimings)
	for _, t := range afterTimings {
		if t.Ready != nil && t.Ready.After(holdStart) {
			res.Flaps++
		}
	}
	h.logf("  delivery attempts by outcome %v", res.DeliveryAttempts)
	h.logf("  %d messages accepted, statuses %v, callbacks %.0f, Ready %d -> %d, restarts %d -> %d, flaps %d",
		client.Requests, res.MessagesByStatus, res.Callbacks, res.ReadyBefore, res.ReadyAfter,
		res.RestartsBefore, res.RestartsAfter, res.Flaps)
	return res, nil
}

func sumRestarts(ts []agentTiming) int32 {
	var n int32
	for _, t := range ts {
		n += t.Restarts
	}
	return n
}

func (h *harness) callbackURL() string {
	return "https://mock-provider." + h.cfg.Namespace + ".svc:8443/callback"
}

// ---- teardown ----

func (h *harness) runTeardown(ctx context.Context) error {
	res, err := h.teardown(ctx, h.rampFleet())
	if err != nil {
		return err
	}
	h.sum.Teardown = res
	return nil
}

// teardown deletes a fleet and its channels and times until the pods are gone.
func (h *harness) teardown(ctx context.Context, f fleet) (*teardownResult, error) {
	timings, err := h.k.agentTimings(ctx, f.scope(), f.phase)
	if err != nil {
		return nil, err
	}
	h.logf("teardown: deleting %d %s agents and their channels", len(timings), f.phase)
	took, err := h.k.deletePhase(ctx, f.namespaces, f.phase, f.prefix, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	h.logf("  gone after %.0fs", took.Seconds())
	return &teardownResult{Agents: len(timings), Seconds: round3(took.Seconds())}, nil
}

// ---- namespaces ----

// namespacesResult is the ramp, hold, and teardown on the same kind of fleet
// spread over many namespaces, so the per-namespace list and RBAC paths that
// one namespace never exercises carry the load. Its blocks compare with the
// standard ramp, hold, and teardown.
type namespacesResult struct {
	Namespaces int             `json:"namespaces"`
	Ramp       *rampResult     `json:"ramp,omitempty"`
	Hold       *holdResult     `json:"hold,omitempty"`
	Teardown   *teardownResult `json:"teardown,omitempty"`
}

// spreadSecrets are copied from the run's namespace into each spread
// namespace, so testdata/infra.yaml stays the one source of their values.
var spreadSecrets = []string{hookSecretName, callbackSecretName}

func (h *harness) runNamespaces(ctx context.Context) error {
	cfg := h.cfg
	ramp, err := h.k.agentTimings(ctx, cfg.Namespace, phaseRamp)
	if err != nil {
		return err
	}
	if len(ramp) > 0 {
		// Both fleets together would pass the machine's pod ceiling.
		return errors.New("namespaces needs the ramp fleet gone: run teardown before it, or run it alone")
	}
	nss := spreadNamespaces(cfg.Namespace, cfg.NamespacesCount)
	// Deferred before preparing: a failure partway through preparing leaves
	// namespaces behind, and deleting an absent or Terminating one is harmless.
	defer h.deleteNamespaces(nss)
	if err := h.prepareNamespaces(ctx, nss); err != nil {
		return err
	}

	res := &namespacesResult{Namespaces: len(nss)}
	h.sum.Namespaces = res
	f := fleet{
		phase: phaseNamespaces, prefix: "spread-", target: cfg.NamespacesAgents,
		namespaces: nss, holdJob: "loadgen-namespaces-hold",
	}
	h.logf("namespaces: %d agents over %d namespaces", f.target, len(nss))
	if res.Ramp, err = h.ramp(ctx, f); err != nil {
		return err
	}
	if res.Hold, err = h.hold(ctx, f); err != nil {
		return err
	}
	res.Teardown, err = h.teardown(ctx, f)
	return err
}

// prepareNamespaces waits out any namespace a previous run left Terminating,
// creates each one with the phase label, copies the channel Secrets in, and
// waits for trust-manager to distribute the CA bundle there.
func (h *harness) prepareNamespaces(ctx context.Context, nss []string) error {
	var terminating []string
	if err := pollUntil(ctx, 10*time.Minute, 5*time.Second, func() (bool, error) {
		terminating = terminating[:0]
		for _, name := range nss {
			var ns corev1.Namespace
			err := h.k.c.Get(ctx, client.ObjectKey{Name: name}, &ns)
			if err == nil && !ns.DeletionTimestamp.IsZero() {
				terminating = append(terminating, name)
			} else if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return len(terminating) == 0, nil
	}); err != nil {
		return fmt.Errorf("namespaces still Terminating from a previous run %v: %w", terminating, err)
	}

	secrets := make([]*corev1.Secret, 0, len(spreadSecrets))
	for _, name := range spreadSecrets {
		var sec corev1.Secret
		if err := h.k.c.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, &sec); err != nil {
			return fmt.Errorf("reading %s/%s to copy: %w", h.cfg.Namespace, name, err)
		}
		secrets = append(secrets, &sec)
	}
	objs := make([]client.Object, 0, len(nss)*(1+len(secrets)))
	for _, name := range nss {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{phaseLabel: phaseNamespaces},
		}})
	}
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	objs = objs[:0]
	for _, name := range nss {
		for _, sec := range secrets {
			objs = append(objs, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: sec.Name, Namespace: name, Labels: sec.Labels},
				Type:       sec.Type,
				Data:       sec.Data,
			})
		}
	}
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	for _, name := range nss {
		if err := pollUntil(ctx, 2*time.Minute, 2*time.Second, func() (bool, error) {
			_, err := h.k.cs.CoreV1().ConfigMaps(name).Get(ctx, "kaalm-ca", metav1.GetOptions{})
			return err == nil, nil
		}); err != nil {
			return fmt.Errorf("trust bundle never reached namespace %s: %w", name, err)
		}
	}
	return nil
}

// deleteNamespaces starts deleting the spread namespaces and does not wait:
// the next run of the phase waits for any still Terminating.
func (h *harness) deleteNamespaces(nss []string) {
	for _, name := range nss {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := h.k.c.Delete(context.Background(), ns); err != nil && !apierrors.IsNotFound(err) {
			h.note("deleting namespace %s: %v", name, err)
		}
	}
}

// ---- providers ----

// providersResult is the control plane and the gateway with many more
// ModelProviders and AgentClasses than the standard run's handful, which
// grows the caches and indexes keyed by them. Its legs compare with the
// gateway table's first two rows.
type providersResult struct {
	Providers         int     `json:"providers"`
	Classes           int     `json:"classes"`
	ProvidersReadySec float64 `json:"providersReadySec"`
	ClassesReadySec   float64 `json:"classesReadySec"`
	// Reconcile is per controller (modelprovider, agentclass) over the
	// setup, from creating the first provider to the last class Ready.
	Reconcile map[string]reconcileFigures `json:"reconcile"`
	// Steady is the control-plane traffic over -idle-duration once all are
	// Ready, with no agents.
	Steady             *apiAudit    `json:"steady,omitempty"`
	ControllerUsageMax usage        `json:"controllerUsageMax"`
	GatewayUsageMax    usage        `json:"gatewayUsageMax"`
	Legs               []gatewayLeg `json:"legs"`
	TeardownSec        float64      `json:"teardownSec"`
}

type reconcileFigures struct {
	Count float64 `json:"count"`
	Ms    stats   `json:"ms"`
}

// The controllers whose reconciles the providers phase reports, by their
// controller-runtime name.
var providersControllers = []string{"modelprovider", "agentclass"}

// manyProviders is the providers phase's ModelProviders: perf-fast's shape,
// each on its own mock prefix, labeled for teardown.
func manyProviders(ns string, n int) []*kaalmv1beta1.ModelProvider {
	out := make([]*kaalmv1beta1.ModelProvider, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("perf-many-%02d", i)
		out = append(out, &kaalmv1beta1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{phaseLabel: phaseProviders}},
			Spec: kaalmv1beta1.ModelProviderSpec{
				Type:           "openai-compatible",
				Endpoint:       fmt.Sprintf("https://mock-provider.%s.svc:8443/ok/many%02d", ns, i),
				CredentialsRef: kaalmv1beta1.SecretKeyReference{Name: "perf-mock-key", Key: "token"},
				HealthCheck:    &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false},
				// The fleet namespace, as on every infra.yaml provider.
				AllowedNamespaces: []string{ns},
				Budget:            kaalmv1beta1.ModelProviderBudget{Period: "daily", PerNamespaceUSD: "1000000"},
				Models: []kaalmv1beta1.ModelProviderModel{{
					ID: "mock-model", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "2.00",
				}},
			},
		})
	}
	return out
}

// manyClasses is one AgentClass per provider, each allowing every provider:
// the widest fan-out for the provider-to-class and class-to-provider watches.
func manyClasses(providers []*kaalmv1beta1.ModelProvider) []*kaalmv1beta1.AgentClass {
	refs := make([]kaalmv1beta1.LocalObjectReference, 0, len(providers))
	for _, p := range providers {
		refs = append(refs, kaalmv1beta1.LocalObjectReference{Name: p.Name})
	}
	out := make([]*kaalmv1beta1.AgentClass, 0, len(providers))
	for _, p := range providers {
		c := activeClass()
		c.Name = p.Name
		c.Labels = map[string]string{phaseLabel: phaseProviders}
		c.Spec.AllowedProviders = append([]kaalmv1beta1.LocalObjectReference(nil), refs...)
		out = append(out, c)
	}
	return out
}

// providersLegs are the gateway table's first two rows under their own job
// names.
func providersLegs() []legSpec {
	out := make([]legSpec, 0, 2)
	for _, d := range gatewayLegs[:2] {
		job := "loadgen-providers-token"
		if d.mtls {
			job = "loadgen-providers-mtls"
		}
		out = append(out, llmLegSpec(d.name, d.provider, job, d.mtls))
	}
	return out
}

// allReady polls until every listed object reports Ready and returns how
// long that took from start.
func (h *harness) allReady(ctx context.Context, what string, start time.Time, list client.ObjectList,
	conditions func() [][]metav1.Condition, want int) (float64, error) {
	ready := 0
	err := pollUntil(ctx, 10*time.Minute, 3*time.Second, func() (bool, error) {
		if err := h.k.c.List(ctx, list, client.MatchingLabels{phaseLabel: phaseProviders}); err != nil {
			return false, err
		}
		ready = 0
		for _, conds := range conditions() {
			if meta.IsStatusConditionTrue(conds, kaalmv1beta1.ConditionReady) {
				ready++
			}
		}
		return ready == want, nil
	})
	if err != nil {
		return 0, fmt.Errorf("%d of %d %s Ready: %w", ready, want, what, err)
	}
	return round3(time.Since(start).Seconds()), nil
}

func (h *harness) runProviders(ctx context.Context) error {
	cfg := h.cfg
	n := cfg.ProvidersCount
	res := &providersResult{Providers: n, Classes: n, Reconcile: map[string]reconcileFigures{}}
	h.sum.Providers = res
	deleted := false
	defer func() {
		if !deleted {
			_, _ = h.deleteProvidersObjects(context.Background(), time.Minute)
		}
	}()

	sampler := h.sampleUsage(ctx)
	err := h.providersSetup(ctx, res)
	peak := sampler.finish()
	res.ControllerUsageMax, res.GatewayUsageMax = peak["kaalm-controller"], peak["kaalm-gateway"]
	if err != nil {
		return err
	}

	if err := h.k.ensureClass(ctx, activeClass()); err != nil {
		return err
	}
	agent := loadgenAgentObj(cfg.Namespace, activeClassName, cfg.AgentImage, gatewayProviders)
	mtlsSecret, cleanup, err := h.loadgenIdentity(ctx, agent)
	if err != nil {
		return err
	}
	defer cleanup()
	legs := providersLegs()
	for i, spec := range legs {
		h.logf("providers leg %d/%d: %s (%d callers, %s)",
			i+1, len(legs), spec.name, cfg.GatewayConcurrency, cfg.GatewayDuration)
		leg, _, err := h.runLeg(ctx, spec, mtlsSecret)
		if err != nil {
			return fmt.Errorf("providers %w", err)
		}
		res.Legs = append(res.Legs, leg)
	}

	took, err := h.deleteProvidersObjects(ctx, 10*time.Minute)
	deleted = true
	if err != nil {
		return err
	}
	res.TeardownSec = round3(took.Seconds())
	h.logf("  providers and classes gone after %.0fs", res.TeardownSec)
	return nil
}

// providersSetup creates the providers, then the classes, records how long
// each set took to turn Ready and what the controllers reconciled meanwhile,
// and counts the control-plane traffic once all of them are Ready.
func (h *harness) providersSetup(ctx context.Context, res *providersResult) error {
	cfg := h.cfg
	n := cfg.ProvidersCount
	ctlBefore, err := h.scrapeController(ctx)
	if err != nil {
		return err
	}

	providers := manyProviders(cfg.Namespace, n)
	objs := make([]client.Object, 0, n)
	for _, p := range providers {
		objs = append(objs, p)
	}
	h.logf("providers: creating %d ModelProviders", n)
	start := time.Now()
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	var mps kaalmv1beta1.ModelProviderList
	res.ProvidersReadySec, err = h.allReady(ctx, "ModelProviders", start, &mps, func() [][]metav1.Condition {
		out := make([][]metav1.Condition, 0, len(mps.Items))
		for i := range mps.Items {
			out = append(out, mps.Items[i].Status.Conditions)
		}
		return out
	}, n)
	if err != nil {
		return err
	}
	h.logf("  all Ready after %.0fs; creating %d AgentClasses that each allow all of them", res.ProvidersReadySec, n)

	objs = objs[:0]
	for _, c := range manyClasses(providers) {
		objs = append(objs, c)
	}
	classStart := time.Now()
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	var classes kaalmv1beta1.AgentClassList
	res.ClassesReadySec, err = h.allReady(ctx, "AgentClasses", classStart, &classes, func() [][]metav1.Condition {
		out := make([][]metav1.Condition, 0, len(classes.Items))
		for i := range classes.Items {
			out = append(out, classes.Items[i].Status.Conditions)
		}
		return out
	}, n)
	if err != nil {
		return err
	}
	h.logf("  all Ready after %.0fs", res.ClassesReadySec)

	ctlAfter, err := h.scrapeController(ctx)
	if err != nil {
		return err
	}
	for _, name := range providersControllers {
		hist := histogramDelta(ctlBefore, ctlAfter, metricReconcileTime, map[string]string{labelController: name})
		fig := reconcileFigures{Ms: histStats(hist)}
		if hist != nil {
			fig.Count = hist.count
		}
		res.Reconcile[name] = fig
		h.logf("  %s: %.0f reconciles over the setup, p50 %.1f ms p99 %.1f ms", name, fig.Count, fig.Ms.P50, fig.Ms.P99)
	}

	if cfg.IdleDuration <= 0 {
		return nil
	}
	h.logf("providers: counting control-plane traffic for %s with all of them Ready", cfg.IdleDuration)
	steadyBefore, err := h.auditSnapshot(ctx)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(cfg.IdleDuration):
	}
	steadyAfter, err := h.auditSnapshot(ctx)
	if err != nil {
		return err
	}
	res.Steady = audit(steadyBefore, steadyAfter, 0)
	h.logAudit("providers steady audit", res.Steady)
	return nil
}

// deleteProvidersObjects deletes the providers phase's AgentClasses and
// ModelProviders and waits until both are gone.
func (h *harness) deleteProvidersObjects(ctx context.Context, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	sel := client.MatchingLabels{phaseLabel: phaseProviders}
	for _, obj := range []client.Object{&kaalmv1beta1.AgentClass{}, &kaalmv1beta1.ModelProvider{}} {
		if err := h.k.c.DeleteAllOf(ctx, obj, sel); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	err := pollUntil(ctx, timeout, 3*time.Second, func() (bool, error) {
		var classes kaalmv1beta1.AgentClassList
		if err := h.k.c.List(ctx, &classes, sel); err != nil {
			return false, err
		}
		var mps kaalmv1beta1.ModelProviderList
		if err := h.k.c.List(ctx, &mps, sel); err != nil {
			return false, err
		}
		return len(classes.Items)+len(mps.Items) == 0, nil
	})
	return time.Since(start), err
}

// ---- churn ----

func (h *harness) runChurn(ctx context.Context) error {
	cfg := h.cfg
	if err := h.k.ensureClass(ctx, churnClass(10*time.Second, 5*time.Second)); err != nil {
		return err
	}
	objs := make([]client.Object, 0, 2*cfg.ChurnAgents)
	for i := 0; i < cfg.ChurnAgents; i++ {
		name := fmt.Sprintf("churn-%04d", i)
		objs = append(objs,
			agentObj(cfg.Namespace, name, churnClassName, cfg.AgentImage, phaseChurn, true),
			channelObj(cfg.Namespace, name, phaseChurn, h.callbackURL()))
	}
	h.logf("churn: creating %d persistence-enabled agents with channels", cfg.ChurnAgents)
	start := time.Now()
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	res := &churnResult{Agents: cfg.ChurnAgents}
	// Ready times are collected across polls: an agent that hibernates
	// before the fleet is complete loses its Ready condition, and with it
	// the transition time, so each is recorded the first time it is seen.
	readyTimes := map[string]time.Time{}
	createdTimes := map[string]time.Time{}
	var timings []agentTiming
	up := 0
	if err := pollUntil(ctx, 10*time.Minute, 3*time.Second, func() (bool, error) {
		ts, err := h.k.agentTimings(ctx, cfg.Namespace, phaseChurn)
		if err != nil {
			return false, err
		}
		timings = ts
		up = 0
		for _, t := range ts {
			createdTimes[t.Name] = t.Created
			if t.Ready != nil {
				if _, seen := readyTimes[t.Name]; !seen {
					readyTimes[t.Name] = *t.Ready
				}
			}
			if cameUp(t) {
				up++
			}
		}
		return up == cfg.ChurnAgents, nil
	}); err != nil {
		return fmt.Errorf("churn fleet never fully came up: %w (%d of %d)", err, up, cfg.ChurnAgents)
	}
	res.AllReadySec = round3(time.Since(start).Seconds())
	var ttr []float64
	for name, ready := range readyTimes {
		ttr = append(ttr, ready.Sub(createdTimes[name]).Seconds())
	}
	res.TimeToReadySec = summarize(ttr)
	h.logf("  all %d Ready after %.0fs; waiting for the first hibernation of every agent",
		cfg.ChurnAgents, res.AllReadySec)
	hibStart := time.Now()
	if err := pollUntil(ctx, 8*time.Minute, 5*time.Second, func() (bool, error) {
		ts, err := h.k.agentTimings(ctx, cfg.Namespace, phaseChurn)
		if err != nil {
			return false, err
		}
		timings = ts
		return countPhase(ts, kaalmv1beta1.AgentHibernated) == cfg.ChurnAgents, nil
	}); err != nil {
		return fmt.Errorf("churn fleet never fully Hibernated: %w (%d of %d)",
			err, countPhase(timings, kaalmv1beta1.AgentHibernated), cfg.ChurnAgents)
	}
	res.AllHibernatedSec = round3(time.Since(hibStart).Seconds())
	var firstHib []float64
	for _, t := range timings {
		if t.Hibernated != nil {
			if r, ok := readyTimes[t.Name]; ok {
				firstHib = append(firstHib, t.Hibernated.Sub(r).Seconds())
			}
		}
	}
	res.FirstHibernationSec = summarize(firstHib)
	h.logf("  all Hibernated after %.0fs (Ready-to-Hibernated p50 %.0fs p95 %.0fs)",
		res.AllHibernatedSec, res.FirstHibernationSec.P50, res.FirstHibernationSec.P95)

	if cfg.IdleDuration > 0 {
		h.logf("idle: %d hibernated agents, counting control-plane traffic for %s", cfg.ChurnAgents, cfg.IdleDuration)
		idleBefore, err := h.auditSnapshot(ctx)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.IdleDuration):
		}
		idleAfter, err := h.auditSnapshot(ctx)
		if err != nil {
			return err
		}
		res.Idle = audit(idleBefore, idleAfter, cfg.ChurnAgents)
		h.logAudit("idle audit", res.Idle)
	}

	gwBefore, err := h.scrapeGateway(ctx)
	if err != nil {
		return err
	}
	ctlBefore, err := h.scrapeController(ctx)
	if err != nil {
		return err
	}
	rate := float64(cfg.ChurnAgents) / cfg.ChurnCycle.Seconds()
	h.logf("churn: %.2f msg/s for %s (one message per agent per %s)", rate, cfg.ChurnDuration, cfg.ChurnCycle)
	client, err := h.k.runLoadgen(ctx, cfg.Namespace, "loadgen-churn", cfg.LoadgenImage, []string{
		flagMode, modeChannels,
		"-path-prefix", "/channels/" + cfg.Namespace + "/churn-",
		"-count", strconv.Itoa(cfg.ChurnAgents),
		"-pad", "4",
		"-rate", strconv.FormatFloat(rate, 'f', 4, 64),
		flagDuration, cfg.ChurnDuration.String(),
	}, "", cfg.ChurnDuration+3*time.Minute)
	if err != nil {
		return err
	}
	res.Client = client
	// The last messages' wakes may take the full wake budget to resolve.
	if err := sleepCtx(ctx, 90*time.Second); err != nil {
		return err
	}
	gwAfter, err := h.scrapeGateway(ctx)
	if err != nil {
		return err
	}
	ctlAfter, err := h.scrapeController(ctx)
	if err != nil {
		return err
	}
	ns := map[string]string{"namespace": cfg.Namespace}
	webhook := map[string]string{"channel_type": channelTypeWebhook}
	res.MessagesByStatus = counterByLabel(gwBefore, gwAfter, "kaalm_channel_messages_total", "status", webhook)
	res.WakeDurationMs = histStats(histogramDelta(gwBefore, gwAfter, "kaalm_channel_wake_duration_seconds", ns))
	res.WakesByResult = histogramCountByLabel(gwBefore, gwAfter, "kaalm_channel_wake_duration_seconds", "result", ns)
	res.WakesTotal = counterDelta(ctlBefore, ctlAfter, "kaalm_wakes_total", ns)
	res.HibernationsTotal = counterDelta(ctlBefore, ctlAfter, "kaalm_hibernations_total", ns)
	res.Callbacks = counterDelta(gwBefore, gwAfter, "kaalm_channel_callback_total", nil)
	h.logf("  %d messages, wakes %.0f (by result %v), hibernations %.0f, wake p50 %.0f ms p95 %.0f ms, callbacks %.0f",
		client.Requests, res.WakesTotal, res.WakesByResult, res.HibernationsTotal,
		res.WakeDurationMs.P50, res.WakeDurationMs.P95, res.Callbacks)

	took, err := h.k.deletePhase(ctx, []string{cfg.Namespace}, phaseChurn, "churn-", 15*time.Minute)
	if err != nil {
		return err
	}
	res.TeardownSec = round3(took.Seconds())
	h.sum.Churn = res
	return nil
}

// histogramCountByLabel returns the delta of a histogram's sample count per
// distinct value of one label (the wake histogram's result label).
func histogramCountByLabel(before, after *snapshot, name, label string, want map[string]string) map[string]float64 {
	out := map[string]float64{}
	if after == nil {
		return out
	}
	for _, smp := range after.metrics[name] {
		if !match(want)(smp.labels) || smp.hist == nil {
			continue
		}
		v := smp.hist.count
		if before != nil {
			if b := before.find(name, smp.labels); b != nil && b.hist != nil {
				v -= b.hist.count
			}
		}
		out[smp.labels[label]] += v
	}
	return out
}

// ---- tasks ----

func (h *harness) runTasks(ctx context.Context) error {
	cfg := h.cfg
	if err := h.k.ensureClass(ctx, activeClass()); err != nil {
		return err
	}
	objs := make([]client.Object, 0, cfg.Tasks)
	for i := 0; i < cfg.Tasks; i++ {
		objs = append(objs, taskObj(cfg.Namespace, fmt.Sprintf("task-%04d", i), activeClassName, cfg.AgentImage))
	}
	h.logf("tasks: submitting %d AgentTasks at once", cfg.Tasks)
	start := time.Now()
	if err := h.k.createAll(ctx, objs); err != nil {
		return err
	}
	res := &tasksResult{Submitted: cfg.Tasks, SubmitSec: round3(time.Since(start).Seconds()), Phases: map[string]int{}}
	var list kaalmv1beta1.AgentTaskList
	terminal := map[kaalmv1beta1.AgentTaskPhase]bool{
		kaalmv1beta1.TaskSucceeded: true, kaalmv1beta1.TaskFailed: true, kaalmv1beta1.TaskTimedOut: true,
	}
	taskLabels := client.MatchingLabels{phaseLabel: phaseTasks}
	err := pollUntil(ctx, cfg.TaskTimeout, 3*time.Second, func() (bool, error) {
		if err := h.k.c.List(ctx, &list, client.InNamespace(cfg.Namespace), taskLabels); err != nil {
			return false, err
		}
		done := 0
		for i := range list.Items {
			if terminal[list.Items[i].Status.Phase] {
				done++
			}
		}
		return done == len(list.Items) && len(list.Items) == cfg.Tasks, nil
	})
	if err != nil && !errors.Is(err, errTimeout) {
		return err
	}
	var provision, run, total []float64
	var first, last time.Time
	for i := range list.Items {
		t := &list.Items[i]
		res.Phases[string(t.Status.Phase)]++
		res.Retries += t.Status.Retries
		c := t.CreationTimestamp.Time
		if first.IsZero() || c.Before(first) {
			first = c
		}
		if t.Status.StartTime != nil {
			provision = append(provision, t.Status.StartTime.Sub(c).Seconds())
		}
		if t.Status.CompletionTime != nil {
			total = append(total, t.Status.CompletionTime.Sub(c).Seconds())
			if t.Status.CompletionTime.After(last) {
				last = t.Status.CompletionTime.Time
			}
			if t.Status.StartTime != nil {
				run = append(run, t.Status.CompletionTime.Sub(t.Status.StartTime.Time).Seconds())
			}
		}
	}
	res.ProvisionSec = summarize(provision)
	res.RunSec = summarize(run)
	res.TotalSec = summarize(total)
	if !last.IsZero() {
		res.MakespanSec = round3(last.Sub(first).Seconds())
		if res.MakespanSec > 0 {
			res.ThroughputPerMin = round3(float64(len(total)) * 60 / res.MakespanSec)
		}
	}
	if errors.Is(err, errTimeout) {
		h.note("tasks: not every task settled within %s; phases %v", cfg.TaskTimeout, res.Phases)
	}
	h.logf("  phases %v, makespan %.0fs, %.1f tasks/min, created-to-completion p50 %.0fs p95 %.0fs, retries %d",
		res.Phases, res.MakespanSec, res.ThroughputPerMin, res.TotalSec.P50, res.TotalSec.P95, res.Retries)
	took, err := h.k.deletePhase(ctx, []string{cfg.Namespace}, phaseTasks, "task-", 10*time.Minute)
	if err != nil {
		return err
	}
	res.TeardownSec = round3(took.Seconds())
	h.sum.Tasks = res
	return nil
}

// runRestart rolls each component with the ramp fleet up: the controller
// alone (rollout wall time, then time to the first reconcile of the new
// leader, which includes the leader-election handoff), and the gateway under
// a 60 s token-tier LLM leg, counting the requests that failed while it
// rolled.
func (h *harness) runRestart(ctx context.Context) error {
	cfg := h.cfg
	timings, err := h.k.agentTimings(ctx, cfg.Namespace, phaseRamp)
	if err != nil {
		return err
	}
	if len(timings) == 0 {
		return errors.New("restart: no ramp fleet; run the ramp phase first")
	}
	res := &restartResult{Agents: len(timings)}

	h.logf("restart: rolling the controller with %d agents up", res.Agents)
	t0 := time.Now()
	if err := h.k.kubectl("-n", "kaalm-system", "rollout", "restart", "deploy/kaalm-controller"); err != nil {
		return err
	}
	if err := h.k.kubectl("-n", "kaalm-system", "rollout", "status",
		"deploy/kaalm-controller", "--timeout=5m"); err != nil {
		return err
	}
	res.ControllerRolloutSec = round3(time.Since(t0).Seconds())
	// The new processes start their counters at zero, so the first reconcile
	// anywhere is the first nonzero total across the replicas.
	if err := pollUntil(ctx, 5*time.Minute, 2*time.Second, func() (bool, error) {
		snap, err := h.scrapeController(ctx)
		if err != nil {
			return false, nil // a scrape can miss a pod mid-roll
		}
		return snap.counter("controller_runtime_reconcile_total", nil) > 0, nil
	}); err != nil {
		return fmt.Errorf("restart: controller never reconciled after the roll: %w", err)
	}
	res.ControllerFirstReconcileSec = round3(time.Since(t0).Seconds())
	h.logf("  controller rolled in %.0fs, first reconcile at %.0fs",
		res.ControllerRolloutSec, res.ControllerFirstReconcileSec)

	h.logf("restart: rolling the gateway under a 60s token-tier leg (%d callers)", cfg.GatewayConcurrency)
	type outcome struct {
		res *loadResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := h.k.runLoadgen(ctx, cfg.Namespace, "loadgen-restart", cfg.LoadgenImage, []string{
			flagMode, modeGateway,
			"-model", providerFast + "/mock-model",
			"-concurrency", strconv.Itoa(cfg.GatewayConcurrency),
			flagDuration, "60s",
		}, "", 4*time.Minute)
		done <- outcome{r, err}
	}()
	time.Sleep(15 * time.Second)
	t1 := time.Now()
	if err := h.k.kubectl("-n", "kaalm-system", "rollout", "restart", "deploy/kaalm-gateway"); err != nil {
		return err
	}
	if err := h.k.kubectl("-n", "kaalm-system", "rollout", "status", "deploy/kaalm-gateway", "--timeout=5m"); err != nil {
		return err
	}
	res.GatewayRolloutSec = round3(time.Since(t1).Seconds())
	out := <-done
	if out.err != nil {
		return fmt.Errorf("restart: gateway leg: %w", out.err)
	}
	res.GatewayClient = out.res
	for status, n := range out.res.Statuses {
		if status != "200" {
			res.GatewayFailed += n
		}
	}
	h.logf("  gateway rolled in %.0fs; %d requests, %d failed during the roll (statuses %v)",
		res.GatewayRolloutSec, out.res.Requests, res.GatewayFailed, out.res.Statuses)
	h.sum.Restart = res
	return nil
}
