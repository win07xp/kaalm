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

package drain

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// harness runs one plain-HTTP traffic server and one health server on
// loopback ports under a Group.
type harness struct {
	traffic, health net.Listener
	drained         atomic.Bool
	done            chan error
	started         time.Time
	cancel          context.CancelFunc
	client          *http.Client
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }

func start(t *testing.T, g Group, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{
		traffic: listen(t), health: listen(t), done: make(chan error, 1),
		// A fresh transport per test, so no connection outlives its test.
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}},
	}
	if handler == nil {
		handler = ok
	}
	g.OnDrain = func() { h.drained.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	traffic := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	health := &http.Server{Handler: http.HandlerFunc(ok), ReadHeaderTimeout: time.Second}
	go func() {
		h.done <- g.Serve(ctx, []Server{{HTTP: traffic, Listener: h.traffic}}, Server{HTTP: health, Listener: h.health})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return")
		}
	})
	h.waitUp(t)
	return h
}

func (h *harness) url(ln net.Listener) string { return "http://" + ln.Addr().String() + "/" }

func (h *harness) waitUp(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := h.client.Get(h.url(h.health)); err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("health server never answered")
}

// drain cancels the context and records when the drain began.
func (h *harness) drain() {
	h.started = time.Now()
	h.cancel()
}

func (h *harness) wait(t *testing.T, within time.Duration) (time.Duration, error) {
	t.Helper()
	select {
	case err := <-h.done:
		h.done <- err // keep it for Cleanup
		return time.Since(h.started), err
	case <-time.After(within):
		t.Fatalf("Serve did not return within %v", within)
		return 0, nil
	}
}

// get sends a request on a new connection, the way a client that was just
// routed to this Pod would.
func get(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestGroup_ServesThroughTheDelay(t *testing.T) {
	h := start(t, Group{Delay: time.Second, Timeout: 5 * time.Second}, nil)
	h.drain()

	time.Sleep(100 * time.Millisecond)
	if !h.drained.Load() {
		t.Error("OnDrain was not called when the drain began")
	}
	resp := get(t, h.url(h.traffic))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("traffic during the delay = %d, want 200", resp.StatusCode)
	}
	if !resp.Close {
		t.Error("a response during the drain must carry Connection: close")
	}
	if resp := get(t, h.url(h.health)); resp.StatusCode != http.StatusOK {
		t.Errorf("health during the delay = %d, want 200", resp.StatusCode)
	}

	took, err := h.wait(t, 5*time.Second)
	if err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if took < time.Second || took > 3*time.Second {
		t.Errorf("Serve returned after %v, want between the 1s delay and 3s", took)
	}
	if conn, err := net.DialTimeout("tcp", h.traffic.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the traffic listener still accepts after Serve returned")
	}
}

func TestGroup_WaitsForInFlightRequests(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	h := start(t, Group{Timeout: 5 * time.Second}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("block") != "" {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte("done"))
	})
	got := make(chan int, 1)
	go func() {
		resp, err := h.client.Get(h.url(h.traffic) + "?block=1")
		if err != nil {
			got <- 0
			return
		}
		_ = resp.Body.Close()
		got <- resp.StatusCode
	}()
	<-entered
	h.drain()

	select {
	case <-h.done:
		t.Fatal("Serve returned while a request was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if code := <-got; code != http.StatusOK {
		t.Errorf("the in-flight request got %d, want 200", code)
	}
	if _, err := h.wait(t, 5*time.Second); err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
}

func TestGroup_TimeoutBoundsTheWait(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	entered := make(chan struct{})
	h := start(t, Group{Timeout: 200 * time.Millisecond}, func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-block
	})
	go func() {
		if resp, err := h.client.Get(h.url(h.traffic)); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	h.drain()
	if took, _ := h.wait(t, 2*time.Second); took < 200*time.Millisecond {
		t.Errorf("Serve returned after %v, before the 200ms timeout", took)
	}
}

func TestGroup_WaitsForPendingWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		release time.Duration
		minTook time.Duration
		maxTook time.Duration
	}{
		{"until the work ends", 5 * time.Second, 300 * time.Millisecond, 300 * time.Millisecond, 3 * time.Second},
		{"until the timeout", 200 * time.Millisecond, 3 * time.Second, 200 * time.Millisecond, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pending sync.WaitGroup
			release := make(chan struct{})
			started := make(chan struct{})
			h := start(t, Group{Timeout: tc.timeout, Pending: &pending}, func(w http.ResponseWriter, _ *http.Request) {
				pending.Add(1)
				go func() {
					defer pending.Done()
					<-release
				}()
				close(started)
				w.WriteHeader(http.StatusAccepted)
			})
			if resp := get(t, h.url(h.traffic)); resp.StatusCode != http.StatusAccepted {
				t.Fatalf("accept = %d", resp.StatusCode)
			}
			<-started
			h.drain()
			timer := time.AfterFunc(tc.release, func() { close(release) })
			t.Cleanup(func() {
				if timer.Stop() {
					close(release)
				}
			})
			took, _ := h.wait(t, tc.maxTook)
			if took < tc.minTook {
				t.Errorf("Serve returned after %v, want at least %v", took, tc.minTook)
			}
		})
	}
}

func TestGroup_HealthAnswersUntilTrafficStops(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	h := start(t, Group{Timeout: 5 * time.Second}, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	})
	go func() {
		if resp, err := h.client.Get(h.url(h.traffic)); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	h.drain()
	time.Sleep(100 * time.Millisecond)
	if resp := get(t, h.url(h.health)); resp.StatusCode != http.StatusOK {
		t.Errorf("health while a request drains = %d, want 200", resp.StatusCode)
	}
	close(release)
	if _, err := h.wait(t, 5*time.Second); err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if conn, err := net.DialTimeout("tcp", h.health.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the health listener still accepts after Serve returned")
	}
}

func TestGroup_ServerFailureSkipsTheDelay(t *testing.T) {
	h := start(t, Group{Delay: 10 * time.Second, Timeout: 5 * time.Second}, nil)
	h.started = time.Now()
	_ = h.traffic.Close()
	took, err := h.wait(t, 3*time.Second)
	if err == nil {
		t.Error("Serve must return the listener error")
	}
	if took > 2*time.Second {
		t.Errorf("Serve took %v after a server failure; the delay must be skipped", took)
	}
	if !h.drained.Load() {
		t.Error("OnDrain must run on a server failure too")
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		delay, timeout time.Duration
		wantErr        bool
	}{
		{"defaults", 5 * time.Second, 30 * time.Second, false},
		{"zeros", 0, 0, false},
		{"negative delay", -time.Second, 30 * time.Second, true},
		{"negative timeout", 5 * time.Second, -time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.delay, tc.timeout); (err != nil) != tc.wantErr {
				t.Errorf("Validate(%v, %v) = %v, want error %v", tc.delay, tc.timeout, err, tc.wantErr)
			}
		})
	}
}
