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
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator's metrics are replica-local: each gateway replica owns its own
// histograms and only the leading controller replica reconciles. A snapshot
// therefore scrapes every pod of a component and merges: counters and
// histograms sum, gauges take the max (the cache-derived fleet gauges are
// identical on every controller replica, and summing would double count).

type sample struct {
	labels map[string]string
	value  float64
	hist   *histogram
}

type histogram struct {
	buckets []bucket // ascending upper bounds, cumulative counts
	count   float64
	sum     float64
}

type bucket struct {
	le    float64
	count float64
}

type snapshot struct {
	at      time.Time
	metrics map[string][]sample // metric name -> samples (one per label set)
	// pods names the Pods the snapshot summed, sorted. Deltas between two
	// snapshots hold only when both summed the same Pods: a Pod that left
	// or joined takes its counters with it.
	pods []string
}

// scrapeComponent snapshots one chart component's /metrics across all its
// pods, retrying a few times: a scrape is a pod list plus a port-forward per
// pod, and either can lose a connection through the k3d load balancer while
// the fleet is large.
func (k *cluster) scrapeComponent(ctx context.Context, namespace, component, port string) (*snapshot, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		snap, err := k.scrapeComponentOnce(ctx, namespace, component, port)
		if err == nil {
			return snap, nil
		}
		lastErr = err
		fmt.Printf("%s  scrape %s attempt %d failed: %v\n", time.Now().Format("15:04:05"), component, attempt+1, err)
		if err := sleepCtx(ctx, time.Duration(attempt+1)*5*time.Second); err != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (k *cluster) scrapeComponentOnce(ctx context.Context, namespace, component, port string) (*snapshot, error) {
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/name":      "kaalm",
		"app.kubernetes.io/component": component,
	}); err != nil {
		return nil, err
	}
	snap := &snapshot{at: time.Now(), metrics: map[string][]sample{}, pods: scrapeTargets(pods.Items)}
	if len(snap.pods) == 0 {
		return nil, fmt.Errorf("no running %s pods to scrape", component)
	}
	for _, name := range snap.pods {
		families, err := k.scrapePod(namespace, name, port)
		if err != nil {
			return nil, fmt.Errorf("scrape %s: %w", name, err)
		}
		snap.merge(families)
	}
	return snap, nil
}

// scrapeTargets returns the sorted names of the Running Pods that are not
// terminating. A gateway or controller replica that is shutting down stays
// Running while it drains; summing it into one snapshot but not the next
// would make that replica's counters look like negative traffic.
func scrapeTargets(pods []corev1.Pod) []string {
	var names []string
	for i := range pods {
		if scrapeable(&pods[i]) {
			names = append(names, pods[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

func scrapeable(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil
}

// componentPods lists one chart component's Pods that scrapeTargets would
// scrape, sorted by name.
func (k *cluster) componentPods(ctx context.Context, namespace, component string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/name":      "kaalm",
		"app.kubernetes.io/component": component,
	}); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for i := range pods.Items {
		if scrapeable(&pods.Items[i]) {
			out = append(out, pods.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// podsChanged reports whether two snapshots summed different Pods, so
// their deltas do not hold. A missing before snapshot changes nothing.
func podsChanged(before, after *snapshot) bool {
	if before == nil || after == nil {
		return false
	}
	return !slices.Equal(before.pods, after.pods)
}

func (k *cluster) scrapePod(namespace, pod, port string) (map[string]*dto.MetricFamily, error) {
	local, stop, err := k.portForward(namespace, "pod/"+pod, port)
	if err != nil {
		return nil, err
	}
	defer stop()
	cli := &http.Client{Timeout: 20 * time.Second}
	var body []byte
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := cli.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", local))
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err == nil && resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty metrics body from %s", pod)
	}
	return parseMetrics(body)
}

// parseMetrics reads a Prometheus text exposition. The parser must be
// built with a name validation scheme: its zero value panics on the first
// metric name.
func parseMetrics(body []byte) (map[string]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(strings.NewReader(string(body)))
}

func (s *snapshot) merge(families map[string]*dto.MetricFamily) {
	for name, mf := range families {
		for _, m := range mf.Metric {
			labels := map[string]string{}
			for _, lp := range m.Label {
				labels[lp.GetName()] = lp.GetValue()
			}
			var incoming sample
			incoming.labels = labels
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				incoming.value = m.Counter.GetValue()
			case dto.MetricType_GAUGE:
				incoming.value = m.Gauge.GetValue()
			case dto.MetricType_HISTOGRAM:
				h := &histogram{count: float64(m.Histogram.GetSampleCount()), sum: m.Histogram.GetSampleSum()}
				for _, b := range m.Histogram.Bucket {
					h.buckets = append(h.buckets, bucket{le: b.GetUpperBound(), count: float64(b.GetCumulativeCount())})
				}
				incoming.hist = h
			default:
				continue
			}
			existing := s.find(name, labels)
			if existing == nil {
				s.metrics[name] = append(s.metrics[name], incoming)
				continue
			}
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				existing.value += incoming.value
			case dto.MetricType_GAUGE:
				existing.value = math.Max(existing.value, incoming.value)
			case dto.MetricType_HISTOGRAM:
				existing.hist.add(incoming.hist)
			}
		}
	}
}

func (s *snapshot) find(name string, labels map[string]string) *sample {
	for i := range s.metrics[name] {
		if labelsEqual(s.metrics[name][i].labels, labels) {
			return &s.metrics[name][i]
		}
	}
	return nil
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (h *histogram) add(o *histogram) {
	h.count += o.count
	h.sum += o.sum
	if len(h.buckets) != len(o.buckets) {
		return // same binary, same buckets; anything else is a scrape mismatch
	}
	for i := range h.buckets {
		h.buckets[i].count += o.buckets[i].count
	}
}

// sub returns h minus o (a delta between two snapshots of the same series).
func (h *histogram) sub(o *histogram) *histogram {
	if o == nil {
		return h
	}
	out := &histogram{count: h.count - o.count, sum: h.sum - o.sum}
	for i, b := range h.buckets {
		c := b.count
		if i < len(o.buckets) {
			c -= o.buckets[i].count
		}
		out.buckets = append(out.buckets, bucket{le: b.le, count: c})
	}
	return out
}

// quantile estimates a quantile from cumulative buckets with linear
// interpolation inside the bucket, the same estimate Prometheus's
// histogram_quantile makes.
func (h *histogram) quantile(q float64) float64 {
	if h == nil || h.count == 0 || len(h.buckets) == 0 {
		return 0
	}
	rank := q * h.count
	prevLE, prevCount := 0.0, 0.0
	for i, b := range h.buckets {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				if i == 0 {
					return 0
				}
				return h.buckets[i-1].le
			}
			if b.count == prevCount {
				return b.le
			}
			return prevLE + (b.le-prevLE)*(rank-prevCount)/(b.count-prevCount)
		}
		prevLE, prevCount = b.le, b.count
	}
	return h.buckets[len(h.buckets)-1].le
}

// histStats renders a histogram delta in milliseconds as a stats block; the
// Prometheus buckets are seconds.
func histStats(h *histogram) stats {
	if h == nil || h.count == 0 {
		return stats{}
	}
	return stats{
		Count: int(h.count),
		Mean:  round3(h.sum / h.count * 1000),
		P50:   round3(h.quantile(0.50) * 1000),
		P90:   round3(h.quantile(0.90) * 1000),
		P95:   round3(h.quantile(0.95) * 1000),
		P99:   round3(h.quantile(0.99) * 1000),
		Max:   round3(maxObservedBucket(h) * 1000),
	}
}

// maxObservedBucket is the upper bound of the highest non-empty finite
// bucket: the tightest statement a histogram can make about its maximum.
func maxObservedBucket(h *histogram) float64 {
	var prev float64
	for _, b := range h.buckets {
		if math.IsInf(b.le, 1) {
			if b.count > prev {
				return h.buckets[len(h.buckets)-2].le // observations beyond the last finite bucket
			}
			break
		}
		prev = b.count
	}
	best := 0.0
	var prevCount float64
	for _, b := range h.buckets {
		if math.IsInf(b.le, 1) {
			break
		}
		if b.count > prevCount {
			best = b.le
		}
		prevCount = b.count
	}
	return best
}

// match selects samples whose labels include every given pair.
func match(want map[string]string) func(map[string]string) bool {
	return func(have map[string]string) bool {
		for k, v := range want {
			if have[k] != v {
				return false
			}
		}
		return true
	}
}

// counter sums matching counter samples in a snapshot.
func (s *snapshot) counter(name string, want map[string]string) float64 {
	if s == nil {
		return 0
	}
	var total float64
	for _, smp := range s.metrics[name] {
		if match(want)(smp.labels) && smp.hist == nil {
			total += smp.value
		}
	}
	return total
}

// counterDelta is after minus before for matching counters.
func counterDelta(before, after *snapshot, name string, want map[string]string) float64 {
	return after.counter(name, want) - before.counter(name, want)
}

// counterByLabel returns after-minus-before per distinct value of one label.
func counterByLabel(before, after *snapshot, name, label string, want map[string]string) map[string]float64 {
	out := map[string]float64{}
	if after == nil {
		return out
	}
	for _, smp := range after.metrics[name] {
		if !match(want)(smp.labels) || smp.hist != nil {
			continue
		}
		v := smp.value
		if before != nil {
			if b := before.find(name, smp.labels); b != nil {
				v -= b.value
			}
		}
		out[smp.labels[label]] += v
	}
	return out
}

// histogramDelta merges every matching histogram series and subtracts the
// before snapshot's matching series.
func histogramDelta(before, after *snapshot, name string, want map[string]string) *histogram {
	if after == nil {
		return nil
	}
	var merged *histogram
	for _, smp := range after.metrics[name] {
		if !match(want)(smp.labels) || smp.hist == nil {
			continue
		}
		delta := smp.hist
		if before != nil {
			if b := before.find(name, smp.labels); b != nil && b.hist != nil {
				delta = smp.hist.sub(b.hist)
			}
		}
		if merged == nil {
			copied := *delta
			copied.buckets = append([]bucket(nil), delta.buckets...)
			merged = &copied
			continue
		}
		merged.add(delta)
	}
	return merged
}

// gauge returns the max of matching gauge samples.
func (s *snapshot) gauge(name string, want map[string]string) float64 {
	if s == nil {
		return 0
	}
	best := 0.0
	for _, smp := range s.metrics[name] {
		if match(want)(smp.labels) && smp.hist == nil {
			best = math.Max(best, smp.value)
		}
	}
	return best
}

// scrapeAPIServer snapshots the apiserver's own /metrics through the API, so
// a window of apiserver_request_total can be read by verb and resource for
// every client of the control plane, kubelets and cert-manager included.
func (k *cluster) scrapeAPIServer(ctx context.Context) (*snapshot, error) {
	raw, err := k.rawGet(ctx, "/metrics")
	if err != nil {
		return nil, err
	}
	fams, err := parseMetrics(raw)
	if err != nil {
		return nil, err
	}
	s := &snapshot{at: time.Now(), metrics: map[string][]sample{}}
	s.merge(fams)
	return s, nil
}

// apiRequestsByVerbResource returns after-minus-before apiserver_request_total
// per "VERB resource" (a subresource appends as resource/sub), every code.
func apiRequestsByVerbResource(before, after *snapshot) map[string]float64 {
	out := map[string]float64{}
	if after == nil {
		return out
	}
	for _, smp := range after.metrics["apiserver_request_total"] {
		if smp.hist != nil {
			continue
		}
		v := smp.value
		if before != nil {
			if b := before.find("apiserver_request_total", smp.labels); b != nil {
				v -= b.value
			}
		}
		if v <= 0 {
			continue
		}
		key := smp.labels["verb"] + " " + smp.labels["resource"]
		if sub := smp.labels["subresource"]; sub != "" {
			key += "/" + sub
		}
		out[key] += v
	}
	return out
}

// topEntries returns the n largest entries of m as "key=value" strings.
func topEntries(m map[string]float64, n int) []string {
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s=%.0f", all[i].k, all[i].v))
	}
	return out
}

// cpuPerRequestMs divides a CPU-seconds delta by a request count, in
// milliseconds per request; 0 when nothing was counted.
func cpuPerRequestMs(cpuSeconds, requests float64) float64 {
	if requests <= 0 {
		return 0
	}
	return round3(cpuSeconds * 1000 / requests)
}

// gaugeSum sums matching gauge samples in a snapshot, where gauge takes the
// max: the apiserver splits one count over label sets such as scope.
func (s *snapshot) gaugeSum(name string, want map[string]string) float64 {
	if s == nil {
		return 0
	}
	var total float64
	for _, smp := range s.metrics[name] {
		if match(want)(smp.labels) && smp.hist == nil {
			total += smp.value
		}
	}
	return total
}

// metricSecretWatches is the apiserver's gauge of open long-running
// requests; its WATCH samples on secrets count the open Secret watches of
// every client, kubelets included. The k8s.io/apiserver this module builds
// against has no apiserver_registered_watchers.
const metricSecretWatches = "apiserver_longrunning_requests"

// secretWatches sums the open Secret watches over every scope, or reports
// false when the apiserver exposes no such gauge.
func secretWatches(s *snapshot) (float64, bool) {
	if s == nil {
		return 0, false
	}
	if _, ok := s.metrics[metricSecretWatches]; !ok {
		return 0, false
	}
	return s.gaugeSum(metricSecretWatches, map[string]string{"verb": "WATCH", "resource": "secrets"}), true
}

// watchGoroutine is the frame of the goroutine client-go runs per open watch
// stream; counting it in a goroutine profile counts a process's watches.
const watchGoroutine = "watch.(*StreamWatcher).receive"

// countGoroutines sums the goroutines in a debug=1 goroutine profile whose
// stack has a frame containing needle. Each block starts "<N> @ 0x..." and
// lists its frames as "#\t0x... <func>+0x...".
func countGoroutines(profile []byte, needle string) int {
	total, n, hit := 0, 0, false
	flush := func() {
		if hit {
			total += n
		}
		n, hit = 0, false
	}
	for _, line := range strings.Split(string(profile), "\n") {
		if count, rest, ok := strings.Cut(line, " @ "); ok && strings.HasPrefix(rest, "0x") {
			flush()
			if v, err := strconv.Atoi(strings.TrimSpace(count)); err == nil {
				n = v
			}
			continue
		}
		if strings.HasPrefix(line, "#") && strings.Contains(line, needle) {
			hit = true
		}
	}
	flush()
	return total
}

// leaderPodName is the Pod in a controller-runtime Lease holder identity,
// "<pod>_<uuid>".
func leaderPodName(holder string) string {
	pod, _, _ := strings.Cut(holder, "_")
	return pod
}

// leaderLease is the controller's leader-election Lease in kaalm-system.
const leaderLease = "e06da714.io"

// controllerLeader names the Pod holding the controller's Lease, or "" when
// nobody holds it.
func (k *cluster) controllerLeader(ctx context.Context) (string, error) {
	lease, err := k.cs.CoordinationV1().Leases("kaalm-system").Get(ctx, leaderLease, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if lease.Spec.HolderIdentity == nil {
		return "", nil
	}
	return leaderPodName(*lease.Spec.HolderIdentity), nil
}

// processFigures is one operator process at one point: Go runtime, RSS, the
// GETs its API client has made since it started, and its open watch streams
// (nil when the goroutine profile could not be read).
type processFigures struct {
	Pod        string  `json:"pod"`
	Leader     bool    `json:"leader,omitempty"`
	Goroutines float64 `json:"goroutines"`
	HeapMiB    float64 `json:"heapMiB"`
	RSSMiB     float64 `json:"rssMiB"`
	APIGets    float64 `json:"apiGets"`
	Watches    *int    `json:"watches,omitempty"`
}

// apiserverFigures is the API server process at one point. On k3s the one
// server process hosts every control-plane component.
type apiserverFigures struct {
	RSSMiB        float64  `json:"rssMiB"`
	CPUSeconds    float64  `json:"cpuSeconds"`
	SecretWatches *float64 `json:"secretWatches,omitempty"`
}

// scaleFigures is every operator process and the API server at one point.
type scaleFigures struct {
	At         time.Time        `json:"at"`
	Gateway    []processFigures `json:"gateway"`
	Controller []processFigures `json:"controller"`
	APIServer  apiserverFigures `json:"apiserver"`
}

// processFiguresFrom reads a process's figures from its /metrics families.
func processFiguresFrom(pod string, families map[string]*dto.MetricFamily) processFigures {
	snap := &snapshot{metrics: map[string][]sample{}}
	snap.merge(families)
	return processFigures{
		Pod:        pod,
		Goroutines: snap.gauge("go_goroutines", nil),
		HeapMiB:    round3(snap.gauge("go_memstats_heap_inuse_bytes", nil) / (1 << 20)),
		RSSMiB:     round3(snap.gauge("process_resident_memory_bytes", nil) / (1 << 20)),
		APIGets:    snap.counter("rest_client_requests_total", map[string]string{"method": "GET"}),
	}
}

// apiserverFiguresFrom reads the API server's figures from a snapshot.
func apiserverFiguresFrom(s *snapshot) apiserverFigures {
	out := apiserverFigures{
		RSSMiB:     round3(s.gauge("process_resident_memory_bytes", nil) / (1 << 20)),
		CPUSeconds: round3(s.counter("process_cpu_seconds_total", nil)),
	}
	if n, ok := secretWatches(s); ok {
		out.SecretWatches = &n
	}
	return out
}

// errNoWatchCount marks figures whose watch count is missing because the
// goroutine profile could not be read; the rest of the figures hold.
var errNoWatchCount = errors.New("no goroutine profile")

// podFigures scrapes one operator Pod's /metrics and, when pprofPort is set,
// counts its open watch streams from its goroutine profile. A profile that
// cannot be read returns the figures without Watches and an error wrapping
// errNoWatchCount.
func (k *cluster) podFigures(ctx context.Context, namespace string, pod corev1.Pod, metricsPort string,
	pprofPort int) (processFigures, error) {
	families, err := k.scrapePod(namespace, pod.Name, metricsPort)
	if err != nil {
		return processFigures{Pod: pod.Name}, err
	}
	fig := processFiguresFrom(pod.Name, families)
	if pprofPort <= 0 {
		return fig, fmt.Errorf("%w: pprof port off", errNoWatchCount)
	}
	profile, err := k.goroutineProfile(ctx, namespace, pod.Name, pprofPort)
	if err != nil {
		return fig, fmt.Errorf("%w from %s: %v", errNoWatchCount, pod.Name, err)
	}
	n := countGoroutines(profile, watchGoroutine)
	fig.Watches = &n
	return fig, nil
}

// goroutineProfile reads a Pod's debug=1 goroutine profile through a
// port-forward to its pprof listener.
func (k *cluster) goroutineProfile(ctx context.Context, namespace, pod string, port int) ([]byte, error) {
	local, stop, err := k.portForward(namespace, "pod/"+pod, strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	defer stop()
	url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", local)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("goroutine profile: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
