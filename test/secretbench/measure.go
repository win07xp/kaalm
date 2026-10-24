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
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// settleHold is how long requests must stay quiet after every watch
	// has opened, so the last confirming GETs land in the counts.
	settleHold = time.Second
	// quietBound caps that wait: reflectors re-open their watches every 5
	// to 10 minutes, so at thousands of Secrets the requests never go fully
	// quiet.
	quietBound    = 30 * time.Second
	updateTimeout = 30 * time.Second
	// updatePoll is how often a read checks for an update; every variant
	// serves it from its cache, so a tight poll costs no requests.
	updatePoll   = 500 * time.Microsecond
	drainTimeout = 60 * time.Second
	// drainSlack is how many goroutines above the pre-variant count still
	// count as drained: client-go and the HTTP/2 transport keep a few.
	drainSlack = 8
)

// sample is one variant's figures at one Secret count.
type sample struct {
	Variant string `json:"variant"`
	Layout  string `json:"layout"`
	Secrets int    `json:"secrets"`
	Repeat  int    `json:"repeat"`

	GoroutinesBefore int   `json:"goroutinesBefore"`
	Goroutines       int   `json:"goroutines"`
	HeapInuseBytes   int64 `json:"heapInuseBytes"`
	RSSBytes         int64 `json:"rssBytes,omitempty"`

	Requests              requestCounts `json:"requests"`
	RequestsDuringUpdates requestCounts `json:"requestsDuringUpdates"`

	ServedSec  float64 `json:"servedSec"`
	SettledSec float64 `json:"settledSec"`
	Settled    bool    `json:"settled"`
	ReadErrors int     `json:"readErrors"`

	UpdateP50Ms  float64 `json:"updateP50Ms"`
	UpdateMaxMs  float64 `json:"updateMaxMs"`
	UpdateMissed int     `json:"updateMissed"`

	APIServerSecretWatches float64 `json:"apiserverSecretWatches"`
	APIServerRSSBytes      int64   `json:"apiserverRSSBytes,omitempty"`
	EtcdRSSBytes           int64   `json:"etcdRSSBytes,omitempty"`

	Notes []string `json:"notes,omitempty"`
}

func (s *sample) note(format string, args ...any) {
	s.Notes = append(s.Notes, fmt.Sprintf(format, args...))
}

// bench is what every measurement shares: the envtest control plane and
// the API server's Secret watch count before any reader ran.
type bench struct {
	base            *rest.Config
	admin           kubernetes.Interface
	pids            controlPlanePIDs
	baselineWatches float64
	cfg             config
}

// measure runs one variant from a cold reader over names, samples it, and
// stops it, waiting for its goroutines and watches to drain so nothing
// leaks into the next variant.
func (b *bench) measure(ctx context.Context, v variant, layout string, repeat int,
	names []types.NamespacedName, nsUsed int) (sample, error) {
	s := sample{Variant: v.Name, Layout: layout, Secrets: len(names), Repeat: repeat}
	s.GoroutinesBefore = runtime.NumGoroutine()

	rc, counter := countedConfig(b.base, v.QPS, v.Burst)
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return s, fmt.Errorf("client for %s: %w", v.Name, err)
	}
	reader := v.Build(ctx, client)
	stopped := false
	defer func() {
		if !stopped {
			reader.Stop()
		}
	}()

	start := time.Now()
	errs, firstErr := coldStart(ctx, reader, names, b.cfg.Readers)
	s.ServedSec = time.Since(start).Seconds()
	s.ReadErrors = errs
	if firstErr != nil {
		s.note("%d cold reads failed; first: %v", errs, firstErr)
	}

	expected := v.ExpectedWatches(len(names), nsUsed)
	s.Settled = waitFor(ctx, start.Add(b.cfg.SettleTimeout), 100*time.Millisecond, func() bool {
		return counter.distinctWatches() >= expected
	})
	s.SettledSec = time.Since(start).Seconds()
	if !s.Settled {
		s.note("%d of %d watches opened within %s", counter.distinctWatches(), expected, b.cfg.SettleTimeout)
	} else if !waitQuiet(ctx, counter) {
		s.note("requests did not go quiet for %s within %s after the watches opened", settleHold, quietBound)
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}

	b.sampleState(ctx, &s, counter)

	before := counter.snapshot()
	b.timeUpdates(ctx, &s, reader, names)
	s.RequestsDuringUpdates = counter.snapshot().minus(before)

	reader.Stop()
	stopped = true
	drained := waitFor(ctx, time.Now().Add(drainTimeout), 200*time.Millisecond, func() bool {
		counter.CloseIdleConnections()
		watches, err := apiServerSecretWatches(ctx, b.admin)
		return err == nil && watches <= b.baselineWatches && runtime.NumGoroutine() <= s.GoroutinesBefore+drainSlack
	})
	if !drained {
		s.note("after Stop, %d goroutines (before: %d) or API server Secret watches above baseline after %s",
			runtime.NumGoroutine(), s.GoroutinesBefore, drainTimeout)
	}
	return s, ctx.Err()
}

// coldStart reads every name once with workers concurrent readers and
// returns how many reads failed and the first failure. A failed read is not
// retried, so a variant that cannot serve shows it rather than hanging.
func coldStart(ctx context.Context, reader secretReader, names []types.NamespacedName, workers int) (int, error) {
	var next, failed atomic.Int64
	var firstErr error
	var once sync.Once
	var wg sync.WaitGroup
	for range min(workers, max(len(names), 1)) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(names) || ctx.Err() != nil {
					return
				}
				if _, err := reader.Get(ctx, names[i].Namespace, names[i].Name); err != nil {
					failed.Add(1)
					once.Do(func() { firstErr = err })
				}
			}
		})
	}
	wg.Wait()
	return int(failed.Load()), firstErr
}

// waitFor polls cond every interval until it holds (true), the deadline
// passes, or ctx ends (false).
func waitFor(ctx context.Context, deadline time.Time, interval time.Duration, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// waitQuiet waits until no GET or LIST is in flight and none has started
// for settleHold, bounded by quietBound.
func waitQuiet(ctx context.Context, counter *countingTransport) bool {
	deadline := time.Now().Add(quietBound)
	last := counter.snapshot()
	quietSince := time.Now()
	return waitFor(ctx, deadline, 100*time.Millisecond, func() bool {
		now := counter.snapshot()
		if now.Get != last.Get || now.List != last.List || counter.inflightNonWatch.Load() != 0 {
			last = now
			quietSince = time.Now()
			return false
		}
		return time.Since(quietSince) >= settleHold
	})
}

// releaseMemory collects garbage and returns the freed pages to the OS. A
// GC alone keeps freed pages until the background scavenger gives them back
// over seconds to minutes, so the RSS of a variant sampled right after
// another would still hold the previous variant's informers.
func releaseMemory() {
	runtime.GC()
	debug.FreeOSMemory()
}

// sampleState records memory, goroutines, requests, and the control plane's
// side after a forced GC that returns freed pages to the OS.
func (b *bench) sampleState(ctx context.Context, s *sample, counter *countingTransport) {
	releaseMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.HeapInuseBytes = int64(ms.HeapInuse)
	s.Goroutines = runtime.NumGoroutine()
	if rss, err := rssBytes(os.Getpid()); err == nil {
		s.RSSBytes = rss
	}
	s.Requests = counter.snapshot()
	if watches, err := apiServerSecretWatches(ctx, b.admin); err == nil {
		s.APIServerSecretWatches = watches - b.baselineWatches
	} else {
		s.note("scrape API server metrics: %v", err)
	}
	if b.pids.APIServer != 0 {
		if rss, err := rssBytes(b.pids.APIServer); err == nil {
			s.APIServerRSSBytes = rss
		}
	}
	if b.pids.Etcd != 0 {
		if rss, err := rssBytes(b.pids.Etcd); err == nil {
			s.EtcdRSSBytes = rss
		}
	}
}

// timeUpdates updates UpdateSamples Secrets, spaced evenly over names, one
// at a time through the admin client, and times each from the write's start
// until the reader returns the new resourceVersion. The figure includes the
// update's round trip, the same for every variant.
func (b *bench) timeUpdates(ctx context.Context, s *sample, reader secretReader, names []types.NamespacedName) {
	n := min(b.cfg.UpdateSamples, len(names))
	if n == 0 {
		return
	}
	var took []float64
	for k := range n {
		key := names[k*len(names)/n]
		// The clock starts before the write: the watch event can reach the
		// reader before the update's own response does.
		start := time.Now()
		rv, err := updateSecret(ctx, b.admin, key.Namespace, key.Name)
		if err != nil {
			s.note("update %s: %v", key, err)
			s.UpdateMissed++
			continue
		}
		seen := waitFor(ctx, start.Add(updateTimeout), updatePoll, func() bool {
			sec, err := reader.Get(ctx, key.Namespace, key.Name)
			return err == nil && sec.ResourceVersion == rv
		})
		if !seen {
			s.UpdateMissed++
			continue
		}
		took = append(took, float64(time.Since(start).Microseconds())/1000)
	}
	if len(took) == 0 {
		return
	}
	slices.Sort(took)
	s.UpdateP50Ms = took[len(took)/2]
	s.UpdateMaxMs = took[len(took)-1]
}
