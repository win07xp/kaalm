//go:build secretbench

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
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
)

// verb is how the benchmark counts a request.
type verb int

const (
	verbOther verb = iota
	verbGet
	verbList
	verbWatch
)

// requestCounts is the requests a reader issued, by verb, plus the ones the
// API server answered 429 Too Many Requests.
type requestCounts struct {
	Get       int64 `json:"get"`
	List      int64 `json:"list"`
	Watch     int64 `json:"watch"`
	Other     int64 `json:"other"`
	Throttled int64 `json:"throttled"`
}

func (c requestCounts) minus(o requestCounts) requestCounts {
	return requestCounts{
		Get: c.Get - o.Get, List: c.List - o.List, Watch: c.Watch - o.Watch,
		Other: c.Other - o.Other, Throttled: c.Throttled - o.Throttled,
	}
}

// classify reads a request's verb from its method and URL: watch=true is a
// WATCH, a GET of a Secret collection is a LIST, a GET of one Secret is a
// GET, and anything else is other.
func classify(req *http.Request) verb {
	if req.Method != http.MethodGet {
		return verbOther
	}
	if w := req.URL.Query().Get("watch"); w == "true" || w == "1" {
		return verbWatch
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	for i, p := range parts {
		if p != "secrets" {
			continue
		}
		switch len(parts) - i {
		case 1:
			return verbList
		case 2:
			return verbGet
		}
	}
	return verbOther
}

// countingTransport counts a reader's requests below its client's rate
// limiter, so it sees what reaches the API server. It also tracks the
// non-watch requests in flight, the watches open now, and the distinct
// watches ever opened (by path and selectors), which tells when every
// Secret's or namespace's watch has opened at least once even while the
// reflectors re-open them.
type countingTransport struct {
	base http.RoundTripper

	get, list, watches, other, throttled atomic.Int64
	inflightNonWatch                     atomic.Int64
	openWatches                          atomic.Int64

	targetsMu sync.Mutex
	targets   map[string]struct{}
}

func newCountingTransport(base http.RoundTripper) *countingTransport {
	return &countingTransport{base: base, targets: map[string]struct{}{}}
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	v := classify(req)
	switch v {
	case verbGet:
		t.get.Add(1)
	case verbList:
		t.list.Add(1)
	case verbWatch:
		t.watches.Add(1)
	default:
		t.other.Add(1)
	}
	if v != verbWatch {
		t.inflightNonWatch.Add(1)
		defer t.inflightNonWatch.Add(-1)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		t.throttled.Add(1)
	}
	if v == verbWatch && resp.StatusCode == http.StatusOK {
		t.openWatches.Add(1)
		t.addTarget(watchTarget(req.URL))
		resp.Body = &watchBody{ReadCloser: resp.Body, open: &t.openWatches}
	}
	return resp, nil
}

// CloseIdleConnections drops the variant's idle connections after Stop, so
// their goroutines do not count against the next variant.
func (t *countingTransport) CloseIdleConnections() { utilnet.CloseIdleConnectionsFor(t.base) }

// WrappedRoundTripper lets client-go unwrap the transport.
func (t *countingTransport) WrappedRoundTripper() http.RoundTripper { return t.base }

func (t *countingTransport) addTarget(key string) {
	t.targetsMu.Lock()
	t.targets[key] = struct{}{}
	t.targetsMu.Unlock()
}

// distinctWatches is how many different watches have opened at least once.
func (t *countingTransport) distinctWatches() int {
	t.targetsMu.Lock()
	defer t.targetsMu.Unlock()
	return len(t.targets)
}

func (t *countingTransport) snapshot() requestCounts {
	return requestCounts{
		Get: t.get.Load(), List: t.list.Load(), Watch: t.watches.Load(),
		Other: t.other.Load(), Throttled: t.throttled.Load(),
	}
}

// watchTarget names what a watch follows: its path and selectors, without
// the resourceVersion and timeout that change on every re-open.
func watchTarget(u *url.URL) string {
	q := u.Query()
	return u.Path + "?fieldSelector=" + q.Get("fieldSelector") + "&labelSelector=" + q.Get("labelSelector")
}

// watchBody counts a watch as closed once, on Close or on the stream's end.
type watchBody struct {
	io.ReadCloser
	open *atomic.Int64
	once sync.Once
}

func (b *watchBody) done() { b.once.Do(func() { b.open.Add(-1) }) }

func (b *watchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.done()
	}
	return n, err
}

func (b *watchBody) Close() error {
	b.done()
	return b.ReadCloser.Close()
}

// countedConfig copies base with the given client limit and a counting
// transport. The Proxy func (no proxy) makes the config uncacheable for
// client-go's transport cache, so each variant gets its own connections
// rather than sharing the admin client's.
func countedConfig(base *rest.Config, qps float32, burst int) (*rest.Config, *countingTransport) {
	cfg := rest.CopyConfig(base)
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	// A client builds its transport once, when it is created, and the
	// wrapper sets the counter's base then. The config is uncacheable, so a
	// second client built from it would build a new base and overwrite the
	// first; the counter keeps the first, so every client from this config
	// counts through one counter and one set of connections.
	counter := newCountingTransport(nil)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		if counter.base == nil {
			counter.base = rt
		}
		return counter
	})
	return cfg, counter
}
