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
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestClassifyVerbs(t *testing.T) {
	cases := []struct {
		method, url string
		want        verb
	}{
		{http.MethodGet, "https://h/api/v1/namespaces/a/secrets/x", verbGet},
		{http.MethodGet, "https://h/api/v1/namespaces/a/secrets?labelSelector=a%3Db", verbList},
		{http.MethodGet, "https://h/api/v1/secrets", verbList},
		{http.MethodGet, "https://h/api/v1/namespaces/a/secrets?watch=true&fieldSelector=metadata.name%3Dx", verbWatch},
		{http.MethodGet, "https://h/version", verbOther},
		{http.MethodPut, "https://h/api/v1/namespaces/a/secrets/x", verbOther},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := classify(req); got != c.want {
			t.Errorf("classify(%s %s) = %v, want %v", c.method, c.url, got, c.want)
		}
	}
}

// fakeRoundTripper answers from a function.
type fakeRoundTripper func(*http.Request) *http.Response

func (f fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req), nil }

func TestCountingTransportCountsAndTracksWatches(t *testing.T) {
	watchReader, watchWriter := io.Pipe()
	defer func() { _ = watchWriter.Close() }()
	calls := 0
	counter := newCountingTransport(fakeRoundTripper(func(req *http.Request) *http.Response {
		calls++
		switch {
		case req.URL.Query().Get("watch") == "true":
			return &http.Response{StatusCode: http.StatusOK, Body: watchReader}
		case calls == 2:
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(""))}
		default:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}
		}
	}))
	do := func(url string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := counter.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	_ = do("https://h/api/v1/namespaces/a/secrets/x").Body.Close()
	_ = do("https://h/api/v1/namespaces/a/secrets/x").Body.Close()
	watch := do("https://h/api/v1/namespaces/a/secrets?watch=true&fieldSelector=metadata.name%3Dx&resourceVersion=5")

	got := counter.snapshot()
	if got != (requestCounts{Get: 2, Watch: 1, Throttled: 1}) {
		t.Fatalf("counts = %+v, want Get 2, Watch 1, Throttled 1", got)
	}
	if n := counter.inflightNonWatch.Load(); n != 0 {
		t.Fatalf("inflight = %d after the GETs returned, want 0", n)
	}
	if n := counter.openWatches.Load(); n != 1 {
		t.Fatalf("open watches = %d while the watch streams, want 1", n)
	}
	if n := counter.distinctWatches(); n != 1 {
		t.Fatalf("distinct watches = %d, want 1", n)
	}
	_ = watch.Body.Close()
	_ = watch.Body.Close()
	if n := counter.openWatches.Load(); n != 0 {
		t.Fatalf("open watches = %d after Close twice, want 0", n)
	}
}

func TestCountingTransportClosesWatchOnStreamEnd(t *testing.T) {
	counter := newCountingTransport(fakeRoundTripper(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("event"))}
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://h/api/v1/namespaces/a/secrets?watch=true", nil)
	resp, err := counter.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if n := counter.openWatches.Load(); n != 0 {
		t.Fatalf("open watches = %d after EOF, want 0", n)
	}
	_ = resp.Body.Close()
	if n := counter.openWatches.Load(); n != 0 {
		t.Fatalf("open watches = %d after EOF and Close, want 0", n)
	}
}

func TestWatchTargetIgnoresVersion(t *testing.T) {
	const base = "https://h/api/v1/namespaces/a/secrets?watch=true"
	a, _ := http.NewRequest(http.MethodGet, base+"&fieldSelector=x&resourceVersion=1&timeoutSeconds=300", nil)
	b, _ := http.NewRequest(http.MethodGet, base+"&fieldSelector=x&resourceVersion=9&timeoutSeconds=500", nil)
	c, _ := http.NewRequest(http.MethodGet, base+"&fieldSelector=y", nil)
	if watchTarget(a.URL) != watchTarget(b.URL) {
		t.Fatal("re-opened watch counted as a new target")
	}
	if watchTarget(a.URL) == watchTarget(c.URL) {
		t.Fatal("watches on different Secrets share a target")
	}
}

// A lean reader builds two clients from one counted config, one for its
// watch streams. Both must count through the same counter and keep the base
// transport the first client built.
func TestCountedConfigKeepsOneBaseTransportAcrossClients(t *testing.T) {
	base := &rest.Config{Host: "https://127.0.0.1:1"}
	cfg, counter := countedConfig(base, 100, 200)
	if _, err := kubernetes.NewForConfig(cfg); err != nil {
		t.Fatal(err)
	}
	first := counter.base
	if first == nil {
		t.Fatal("first client did not set the counter's base transport")
	}
	if _, err := kubernetes.NewForConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if counter.base != first {
		t.Fatal("second client replaced the counter's base transport")
	}
}
