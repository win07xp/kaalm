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

// Command secretbench is a component-level scale benchmark of the Secret
// watcher (internal/secretwatch) and three prototype readers. It runs against
// an envtest control plane, a real kube-apiserver and etcd from the envtest
// assets, with no cluster and no agent Pods, so its figures are the
// readers' own cost per Secret, which the perf harness's cluster run cannot
// separate from everything else a gateway or controller holds.
//
// The benchmark runs as envtest's cluster admin: it measures cost, not RBAC.
// The prototypes list Secrets, which the shipped grants do not allow. The
// admin is in system:masters, which API priority and fairness exempts, so
// the API server never queues these requests; at the client limits used (at
// most a few hundred QPS) a service account would not meet its seat limits
// either, but queueing and RBAC stay with the perf harness's restart phase.
//
// The variants, each a reader with the Watcher's Get(ctx, namespace, name):
//
//	today-gw    the shipped Watcher at the gateway's client limit (QPS 100, burst 200)
//	today-ctrl  the shipped Watcher at the controller's (QPS 20, burst 30)
//	B           the shipped Watcher at -b-qps and -b-burst
//	C-gw        one informer per Secret that LISTs by metadata.name and
//	C-ctrl      WATCHes from the LIST's version, at the two limits above
//	lean        C's requests (a LIST by name, a WATCH from its version) with no
//	            client-go informer: one shared cache, one goroutine per Secret,
//	            at the gateway's limit
//	A           one informer per namespace on the kaalm.io/channel-credential
//	            label, at the controller's limit
//
// lean exists to split today's per-Secret cost between client-go's informer
// machinery and the watch stream itself. It is held against C-gw to these
// marks: about 30 KiB RSS per Secret or less, at most 2 goroutines per
// Secret (its loop and net/http's per-stream goroutine), and cold start and
// update delivery no slower than C-gw.
//
// One run steps the Secret count up inside one API server (-counts), once
// per layout (every Secret in one namespace, or spread over -namespaces),
// and repeats the whole sweep (-repeats). The Secret set grows in place
// between counts, so every variant at a step sees the same API server
// state; each variant starts from a cold reader and is stopped and drained
// before the next one starts. At each step and variant it records the Go
// heap after a GC, goroutines, the process RSS, requests by verb, the time
// until every Secret is served and until every watch has opened, the time an
// update takes to reach the reader, the API server's open Secret watches,
// and the RSS of kube-apiserver and etcd.
//
// The summary JSON (-out, by default test/secretbench/results/<UTC
// time>.json) is rewritten after every step, so an interrupted run keeps
// what it measured. The table printed at the end gives each figure's
// least-squares slope per Secret and its mean at the largest count.
//
// `make secretwatch-bench` runs it. The secretbench build tag keeps the
// package out of `go build ./...`, `make test`, and CI; `make lint` reads it
// through the build tags in .golangci.yml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/win07xp/kaalm/internal/testenv"
)

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "secretbench:", err)
		}
		os.Exit(2)
	}
	// client-go logs every client-side wait over a second, which at the
	// controller's 20 QPS is most cold reads; the sample's request counts
	// and notes carry what matters.
	klog.SetLogger(logr.Discard())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg, os.Stdout)
	stop()
	if err != nil {
		log.Printf("secretbench: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, out io.Writer) (err error) {
	env := &envtest.Environment{}
	apiServer := env.ControlPlane.GetAPIServer()
	apiServer.Out, apiServer.Err = io.Discard, io.Discard
	restCfg, err := testenv.Start(env)
	if err != nil {
		return fmt.Errorf("start envtest (is KUBEBUILDER_ASSETS set?): %w", err)
	}
	defer func() {
		if stopErr := env.Stop(); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop envtest: %w", stopErr))
		}
	}()

	s := &summary{StartedAt: time.Now().UTC(), Config: cfg}
	b, err := newBench(ctx, restCfg, cfg, s)
	if err != nil {
		return err
	}
	if version, verr := b.admin.Discovery().ServerVersion(); verr == nil {
		s.Kubernetes = version.GitVersion
	}
	log.Printf("secretbench: envtest up (%s); writing %s", s.Kubernetes, cfg.Out)

	err = sweep(ctx, b, s)
	s.FinishedAt = time.Now().UTC()
	if werr := s.write(cfg.Out); werr != nil {
		err = errors.Join(err, werr)
	}
	printTable(out, s)
	return err
}

// newBench builds the admin client, finds the control-plane processes, and
// reads the API server's Secret watch baseline.
func newBench(ctx context.Context, restCfg *rest.Config, cfg config, s *summary) (*bench, error) {
	// No client-side limit and no counting transport: creating Secrets,
	// updates, and scrapes never show in a variant's figures.
	adminCfg := rest.CopyConfig(restCfg)
	adminCfg.QPS, adminCfg.Burst = -1, 0
	admin, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		return nil, fmt.Errorf("admin client: %w", err)
	}
	pids, err := findControlPlane()
	if err != nil {
		s.Notes = append(s.Notes, "no control-plane RSS: "+err.Error())
	}
	baseline, err := apiServerSecretWatches(ctx, admin)
	if err != nil {
		return nil, fmt.Errorf("scrape API server metrics: %w", err)
	}
	s.BaselineSecretWatches = baseline
	if err := ensureNamespaces(ctx, admin, cfg.Layouts, cfg.Namespaces); err != nil {
		return nil, err
	}
	return &bench{base: restCfg, admin: admin, pids: pids, baselineWatches: baseline, cfg: cfg}, nil
}

// sweep runs repeat > layout > count > variant, writing the summary after
// every step.
func sweep(ctx context.Context, b *bench, s *summary) error {
	cfg := b.cfg
	var all []string
	for _, layout := range cfg.Layouts {
		all = append(all, layoutNamespaces(layout, cfg.Namespaces)...)
	}
	run := selectVariants(cfg)
	for repeat := 1; repeat <= cfg.Repeats; repeat++ {
		for _, layout := range cfg.Layouts {
			if err := clearSecrets(ctx, b.admin, all); err != nil {
				return err
			}
			have := 0
			for _, count := range cfg.Counts {
				if err := ensureSecrets(ctx, b.admin, layout, cfg.Namespaces, have, count); err != nil {
					return err
				}
				have = count
				names := secretKeys(count, layout, cfg.Namespaces)
				used := namespacesUsed(count, layout, cfg.Namespaces)
				for _, v := range run {
					log.Printf("secretbench: repeat %d/%d, layout %s, %d Secrets, variant %s",
						repeat, cfg.Repeats, layout, count, v.Name)
					smp, err := b.measure(ctx, v, layout, repeat, names, used)
					if err != nil && ctx.Err() != nil {
						// Interrupted mid-step: the partial sample would
						// skew the fit, so the summary keeps the finished
						// steps only.
						return err
					}
					s.Samples = append(s.Samples, smp)
					if werr := s.write(cfg.Out); werr != nil {
						return errors.Join(err, werr)
					}
					if err != nil {
						return err
					}
					log.Printf("secretbench:   served %.1fs, settled %.1fs, GET %d LIST %d WATCH %d, heap %.1f MiB, %d goroutines",
						smp.ServedSec, smp.SettledSec, smp.Requests.Get, smp.Requests.List, smp.Requests.Watch,
						float64(smp.HeapInuseBytes)/(1<<20), smp.Goroutines)
				}
			}
		}
	}
	return nil
}
