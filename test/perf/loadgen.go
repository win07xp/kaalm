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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The in-cluster load generator. It runs as a Job in the fleet's namespace,
// authenticates the way a workload would (a projected ServiceAccount token for
// the LLM listener, the channel's bearer secret for the user listener), and
// prints one RESULT_JSON line the orchestrator parses from the pod log.

const resultMarker = "RESULT_JSON "

type loadResult struct {
	Mode        string         `json:"mode"`
	Requests    int            `json:"requests"`
	Statuses    map[string]int `json:"statuses"`
	DurationSec float64        `json:"durationSec"`
	RPS         float64        `json:"rps"`
	// LatencyMs is per request; in stream mode it is time to last byte, over
	// the answers that ended (a non-200, or a stream read to its terminator).
	LatencyMs stats `json:"latencyMs"`
	// TTFBMs is time to first byte of each streamed 200, stream mode only.
	TTFBMs    *stats   `json:"ttfbMs,omitempty"`
	Errors    []string `json:"errors,omitempty"`
	WarmupSec float64  `json:"warmupSec"`
}

// Request body formats the gateway mode speaks.
const (
	formatOpenAI    = "openai"
	formatAnthropic = "anthropic"
)

// labelIncomplete counts a streamed 200 that ended before its terminator.
const labelIncomplete = "incomplete"

type loadgenConfig struct {
	mode        string
	url         string
	model       string
	concurrency int
	duration    time.Duration
	tokenFile   string
	caFile      string
	bearerFile  string
	base        string
	pathPrefix  string
	count       int
	pad         int
	rate        float64
	warmup      time.Duration
	noKeepalive bool
	certFile    string
	keyFile     string
	toolURL     string
	tool        string
	format      string
	stream      bool
	namespaces  []string
}

func runLoadgen(args []string) error {
	var c loadgenConfig
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.StringVar(&c.mode, "mode", modeGateway,
		"gateway (LLM proxy at fixed concurrency), tools (MCP tools/call through the broker at fixed concurrency), "+
			"or channels (webhook messages at a fixed rate)")
	fs.StringVar(&c.url, "url", gatewayBase+"/v1/chat/completions",
		"gateway mode: LLM endpoint")
	fs.StringVar(&c.model, "model", providerFast+"/mock-model", "gateway mode: qualified provider/model")
	fs.StringVar(&c.format, "format", formatOpenAI,
		"gateway mode: request body shape, openai (chat completions) or anthropic (messages; pair it with -url)")
	fs.BoolVar(&c.stream, "stream", false,
		"gateway mode: ask for a streamed answer and read it to [DONE] or message_stop, "+
			"recording time to first and last byte")
	fs.IntVar(&c.concurrency, "concurrency", 32, "gateway and tools modes: concurrent callers")
	fs.DurationVar(&c.duration, "duration", time.Minute, "measured run length after warmup")
	fs.StringVar(&c.tokenFile, "token-file", "/var/run/token/token",
		"projected ServiceAccount token (audience kaalm-gateway)")
	fs.StringVar(&c.caFile, "ca-file", "/var/run/ca/ca.crt", "the kaalm-ca bundle the gateway's certificates chain to")
	fs.StringVar(&c.bearerFile, "bearer-file", "/var/run/hook/token", "channels mode: the webhook bearer secret")
	fs.StringVar(&c.base, "base", "https://kaalm-gateway.kaalm-system.svc:8080", "channels mode: user listener base URL")
	fs.StringVar(&c.pathPrefix, "path-prefix", "/channels/perf/ramp-",
		"channels mode: channel path prefix; the index is appended, and {ns} is replaced as -namespaces says")
	var namespaces string
	fs.StringVar(&namespaces, "namespaces", "",
		"channels mode: comma-separated namespaces; channel i's {ns} is namespace i modulo the list length")
	fs.IntVar(&c.count, "count", 1, "channels mode: number of channels under the prefix")
	fs.IntVar(&c.pad, "pad", 4, "channels mode: zero-padding width of the index")
	fs.Float64Var(&c.rate, "rate", 1, "channels mode: messages per second across all channels, round-robin")
	fs.DurationVar(&c.warmup, "warmup", 90*time.Second, "how long to retry until the first success before measuring")
	fs.BoolVar(&c.noKeepalive, "no-keepalive", false,
		"open a fresh connection per request (measures the cost of an in-cluster dial: DNS, TCP, TLS)")
	fs.StringVar(&c.certFile, "cert-file", "",
		"gateway and tools modes: present this client certificate (a Kaalm agent identity) "+
			"instead of the ServiceAccount token")
	fs.StringVar(&c.keyFile, "key-file", "", "gateway and tools modes: the client certificate's key")
	fs.StringVar(&c.toolURL, "tool-url", gatewayBase+"/v1/mcp/"+toolProviderName,
		"tools mode: the broker endpoint of the ToolProvider")
	fs.StringVar(&c.tool, "tool", toolName, "tools mode: the tool every tools/call names")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if namespaces != "" {
		c.namespaces = strings.Split(namespaces, ",")
	}
	if c.format != formatOpenAI && c.format != formatAnthropic {
		return fmt.Errorf("unknown -format %q (openai or anthropic)", c.format)
	}

	ca, err := os.ReadFile(c.caFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("no certificates in %s", c.caFile)
	}
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if c.certFile != "" {
		cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
		if err != nil {
			return fmt.Errorf("client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	transport := &http.Transport{
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   c.noKeepalive,
	}
	cli := &http.Client{Transport: transport, Timeout: 60 * time.Second}

	var res *loadResult
	switch c.mode {
	case modeGateway:
		res, err = gatewayLoad(cli, c)
	case modeTools:
		res, err = toolsLoad(cli, c)
	case modeChannels:
		res, err = channelLoad(cli, c)
	default:
		return fmt.Errorf("unknown mode %q", c.mode)
	}
	if err != nil {
		return err
	}
	out, _ := json.Marshal(res)
	fmt.Println(resultMarker + string(out))
	return nil
}

// recorder collects per-request outcomes from concurrent workers.
type recorder struct {
	mu        sync.Mutex
	latencies []float64
	ttfbs     []float64
	stream    bool
	statuses  map[string]int
	errors    []string
}

func newRecorder() *recorder { return &recorder{statuses: map[string]int{}} }

func (r *recorder) record(status int, latency time.Duration, err error) {
	r.recordLabel(strconv.Itoa(status), latency, err)
}

// recordLabel counts one outcome under a status label: the HTTP status, or a
// finer label such as "rpc_error" for a JSON-RPC error inside a 200.
func (r *recorder) recordLabel(label string, latency time.Duration, err error) {
	r.recordOutcome(outcome{label: label, latency: latency, err: err})
}

// recordStream counts one streamed request; see streamOutcome.
func (r *recorder) recordStream(status int, ttfb, ttlb time.Duration, complete bool, err error) {
	r.recordOutcome(streamOutcome(status, ttfb, ttlb, complete, err))
}

func (r *recorder) recordOutcome(o outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o.stream {
		r.stream = true
	}
	if o.err != nil {
		r.statuses["error"]++
		if len(r.errors) < 5 {
			r.errors = append(r.errors, o.err.Error())
		}
		return
	}
	r.statuses[o.label]++
	if o.label != labelIncomplete {
		r.latencies = append(r.latencies, ms(o.latency))
	}
	if o.ttfb > 0 {
		r.ttfbs = append(r.ttfbs, ms(o.ttfb))
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (r *recorder) result(mode string, elapsed, warmup time.Duration) *loadResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, n := range r.statuses {
		total += n
	}
	res := &loadResult{
		Mode:        mode,
		Requests:    total,
		Statuses:    r.statuses,
		DurationSec: round3(elapsed.Seconds()),
		RPS:         round3(float64(total) / elapsed.Seconds()),
		LatencyMs:   summarize(r.latencies),
		Errors:      r.errors,
		WarmupSec:   round3(warmup.Seconds()),
	}
	if r.stream {
		ttfb := summarize(r.ttfbs)
		res.TTFBMs = &ttfb
	}
	return res
}

func doRequest(cli *http.Client, req *http.Request) (int, time.Duration, error) {
	start := time.Now()
	resp, err := cli.Do(req)
	if err != nil {
		return 0, time.Since(start), err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return resp.StatusCode, time.Since(start), nil
}

// outcome is one request's result under its status label. A streamed
// request also carries its time to first byte; its latency is time to last
// byte.
type outcome struct {
	label   string
	latency time.Duration
	ttfb    time.Duration
	stream  bool
	err     error
}

// streamOutcome labels a streamed request: a 200 whose stream ended before
// its terminator is "incomplete", never a success.
func streamOutcome(status int, ttfb, ttlb time.Duration, complete bool, err error) outcome {
	o := outcome{label: strconv.Itoa(status), latency: ttlb, ttfb: ttfb, stream: true, err: err}
	if err == nil && status == http.StatusOK && !complete {
		o.label = labelIncomplete
	}
	return o
}

// statusOutcome labels a plain HTTP exchange by its status code.
func statusOutcome(status int, latency time.Duration, err error) outcome {
	return outcome{label: strconv.Itoa(status), latency: latency, err: err}
}

// gatewayBearer is the token-tier credential: the projected ServiceAccount
// token, or "" for an mTLS caller, which its certificate identifies.
func gatewayBearer(c loadgenConfig) (string, error) {
	if c.certFile != "" {
		return "", nil
	}
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(token)), nil
}

// fixedConcurrency warms up until one clean 200, then runs c.concurrency
// workers for c.duration, each issuing the next request as soon as the
// previous one returns. The gateway's source-IP cross-check answers 401
// until its Pod informer has seen a freshly created caller; a real client
// retries the same way, and nothing is measured until the first 200.
func fixedConcurrency(c loadgenConfig, mode string, once func() outcome) (*loadResult, error) {
	warmStart := time.Now()
	for {
		o := once()
		if o.err == nil && o.label == "200" {
			break
		}
		if time.Since(warmStart) > c.warmup {
			return nil, fmt.Errorf("warmup: no 200 within %s (last status %s, err %v)", c.warmup, o.label, o.err)
		}
		time.Sleep(2 * time.Second)
	}
	warmup := time.Since(warmStart)

	rec := newRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), c.duration)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < c.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				rec.recordOutcome(once())
			}
		}()
	}
	wg.Wait()
	return rec.result(mode, time.Since(start), warmup), nil
}

// llmRequestBody is one short user message to model, in the OpenAI chat or
// the Anthropic messages shape (which requires max_tokens), streamed when
// asked.
func llmRequestBody(format, model string, stream bool) []byte {
	body := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "load"}},
	}
	if format == formatAnthropic {
		body["max_tokens"] = 64
	}
	if stream {
		body["stream"] = true
	}
	raw, _ := json.Marshal(body)
	return raw
}

// readStream reads an SSE body line by line until the format's terminator:
// data: [DONE] for OpenAI, the message_stop event for Anthropic. ttfb is
// taken at the first non-empty line and ttlb at the terminator, both from
// start. A body that ends first is incomplete.
func readStream(r io.Reader, format string, start time.Time) (ttfb, ttlb time.Duration, complete bool, err error) {
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && ttfb == 0 {
			ttfb = time.Since(start)
		}
		if streamTerminator(format, trimmed) {
			return ttfb, time.Since(start), true, nil
		}
		if readErr == io.EOF {
			return ttfb, time.Since(start), false, nil
		}
		if readErr != nil {
			return ttfb, time.Since(start), false, readErr
		}
	}
}

// streamTerminator reports whether an SSE line ends the stream.
func streamTerminator(format, line string) bool {
	if format != formatAnthropic {
		return line == "data: [DONE]"
	}
	if line == "event: message_stop" {
		return true
	}
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) == nil && ev.Type == "message_stop"
}

// doStream sends a streaming request and reads a 200 to its terminator. A
// non-200 body is drained like doRequest's, and its latency is the whole
// exchange.
func doStream(cli *http.Client, req *http.Request, format string) outcome {
	start := time.Now()
	resp, err := cli.Do(req)
	if err != nil {
		return streamOutcome(0, 0, time.Since(start), false, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return streamOutcome(resp.StatusCode, 0, time.Since(start), false, nil)
	}
	ttfb, ttlb, complete, err := readStream(resp.Body, format, start)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return streamOutcome(resp.StatusCode, ttfb, ttlb, complete, err)
}

// gatewayLoad drives LLM requests at fixed concurrency, streamed or not.
func gatewayLoad(cli *http.Client, c loadgenConfig) (*loadResult, error) {
	bearer, err := gatewayBearer(c)
	if err != nil {
		return nil, err
	}
	body := llmRequestBody(c.format, c.model, c.stream)
	newReq := func() *http.Request {
		req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return req
	}
	once := func() outcome {
		if c.stream {
			return doStream(cli, newReq(), c.format)
		}
		return statusOutcome(doRequest(cli, newReq()))
	}
	return fixedConcurrency(c, modeGateway, once)
}

// mcpRevision is the MCP protocol revision the tools mode speaks: the
// stateless one, with no initialize and no session.
const mcpRevision = "2026-07-28"

// toolCallBody is a self-describing tools/call request on mcpRevision.
func toolCallBody(tool string) []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": tool, "arguments": map[string]any{},
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcpRevision},
		},
	})
	return body
}

// toolCallStatus labels a broker answer: "rpc_error" when an HTTP 200
// carries a JSON-RPC error (the tool server refused the call), the HTTP
// status otherwise.
func toolCallStatus(httpStatus int, body []byte) string {
	if httpStatus == http.StatusOK {
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && len(env.Error) > 0 && string(env.Error) != "null" {
			return "rpc_error"
		}
	}
	return strconv.Itoa(httpStatus)
}

// toolsLoad drives tools/call through the gateway's MCP broker at fixed
// concurrency, with the same auth choice as gatewayLoad.
func toolsLoad(cli *http.Client, c loadgenConfig) (*loadResult, error) {
	bearer, err := gatewayBearer(c)
	if err != nil {
		return nil, err
	}
	body := toolCallBody(c.tool)
	once := func() outcome {
		req, _ := http.NewRequest(http.MethodPost, c.toolURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", mcpRevision)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", c.tool)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		start := time.Now()
		resp, err := cli.Do(req)
		if err != nil {
			return outcome{latency: time.Since(start), err: err}
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		return outcome{label: toolCallStatus(resp.StatusCode, raw), latency: time.Since(start)}
	}
	return fixedConcurrency(c, modeTools, once)
}

// channelURL is channel i's webhook URL: the prefix with {ns} replaced by
// namespace i modulo the list (when there is a list), then the index
// zero-padded to pad digits. The fleet places agent i the same way.
func channelURL(base, pathPrefix string, namespaces []string, pad, i int) string {
	if len(namespaces) > 0 {
		pathPrefix = strings.ReplaceAll(pathPrefix, "{ns}", namespaces[i%len(namespaces)])
	}
	return fmt.Sprintf("%s%s%0*d", base, pathPrefix, pad, i)
}

// channelLoad posts webhook messages round-robin across the channels at a
// fixed aggregate rate, so message i lands on channel i mod count. With rate =
// count/interval every channel receives exactly one message per interval,
// which is how the hold phase keeps the whole fleet lightly served and the
// churn phase makes nearly every message a cold wake.
func channelLoad(cli *http.Client, c loadgenConfig) (*loadResult, error) {
	bearerRaw, err := os.ReadFile(c.bearerFile)
	if err != nil {
		return nil, err
	}
	bearer := strings.TrimSpace(string(bearerRaw))
	if c.count <= 0 || c.rate <= 0 {
		return nil, fmt.Errorf("channels mode needs count > 0 and rate > 0")
	}
	newReq := func(i int) *http.Request {
		body := fmt.Sprintf(`{"userId":"load","content":{"text":"ping %d"}}`, i)
		req, _ := http.NewRequest(http.MethodPost, channelURL(c.base, c.pathPrefix, c.namespaces, c.pad, i),
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		return req
	}

	// Warmup against channel 0 until the gateway accepts (202 for async).
	warmStart := time.Now()
	for {
		status, _, err := doRequest(cli, newReq(0))
		if err == nil && (status == http.StatusAccepted || status == http.StatusOK) {
			break
		}
		if time.Since(warmStart) > c.warmup {
			return nil, fmt.Errorf("warmup: channel 0 never accepted within %s (last status %d, err %v)", c.warmup, status, err)
		}
		time.Sleep(2 * time.Second)
	}
	warmup := time.Since(warmStart)

	rec := newRecorder()
	interval := time.Duration(float64(time.Second) / c.rate)
	workers := int(c.rate*3) + 4
	if workers > 128 {
		workers = 128
	}
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				status, lat, err := doRequest(cli, newReq(i))
				rec.record(status, lat, err)
			}
		}()
	}
	start := time.Now()
	ticker := time.NewTicker(interval)
	next := 0
	for time.Since(start) < c.duration {
		<-ticker.C
		select {
		case jobs <- next % c.count:
		default:
			rec.record(0, 0, fmt.Errorf("workers saturated at %.1f msg/s", c.rate))
		}
		next++
	}
	ticker.Stop()
	close(jobs)
	wg.Wait()
	return rec.result(modeChannels, time.Since(start), warmup), nil
}
