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

// Command secretbench measures the cost per Secret of the shipped Secret
// watcher (internal/secretwatch.Watcher), which runs one name-selected LIST
// and WATCH informer per Secret it serves. It runs against an envtest
// control plane, a real kube-apiserver and etcd from the envtest assets,
// with no cluster and no agent Pods, so its figures are the Watcher's own,
// which the perf harness's cluster run cannot separate from everything else
// a gateway or controller holds.
//
// The Watcher runs at -qps and -burst, by default the gateway's client limit
// (QPS 100, burst 200), which the controller's Secret watcher also runs at.
// The sweep runs as envtest's cluster admin. The admin is in
// system:masters, which API priority and fairness exempts, so the API
// server never queues these requests; at a few hundred QPS a service
// account would not meet its seat limits either, but queueing stays with
// the perf harness's restart phase.
//
// One run steps the Secret count up inside one API server (-counts), once
// per layout (every Secret in one namespace, or spread over -namespaces),
// and repeats the whole sweep (-repeats). The Secret set grows in place
// between counts. Each step starts a cold Watcher, reads every Secret once
// with -readers concurrent reads, waits until every Secret's watch has
// opened and requests stay quiet for 1 s, samples, times -update-samples
// updates, and stops the Watcher, waiting for it to drain before the next
// step. At each step it records the Go heap after a GC, goroutines, the
// process RSS, requests by verb and the 429s among them, the time until
// every Secret is served and until every watch has opened, the time an
// update takes to reach the Watcher, the API server's open Secret watches,
// and the RSS of kube-apiserver and etcd. measure.go says how each figure
// is read.
//
// The summary JSON (-out, by default test/secretbench/results/<UTC
// time>.json) is rewritten after every step, so an interrupted run keeps
// what it measured. The table printed at the end gives, per layout, each
// figure's least-squares slope per Secret and its mean at the largest count.
//
// -rbac-check runs instead of the sweep. It runs the Watcher as a user bound
// to a Role that grants get, list, and watch on named Secrets only, the
// shape of the Roles the controller mints, through the API server's RBAC
// authorizer, and checks that (a) granted names are served from the watch,
// (b) a LIST or WATCH without a name selector is refused, (c) a
// name-selected LIST or WATCH on an ungranted name is refused, and (d) once
// a name leaves the Role the Watcher stops serving it and answers the
// refusal within 30 s, with the Watcher's watch timeout cut to 5 s. It
// prints a PASS or FAIL line per check and exits non-zero when any check
// fails.
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
	// client-go logs every client-side wait over a second, which at a low
	// -qps is most cold reads; the sample's request counts and notes carry
	// what matters.
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
	if cfg.RBACCheck {
		return rbacCheck(ctx, env, restCfg, out)
	}

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
	// updates, and scrapes never show in the Watcher's figures.
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

// sweep runs repeat > layout > count, writing the summary after every
// step.
func sweep(ctx context.Context, b *bench, s *summary) error {
	cfg := b.cfg
	var all []string
	for _, layout := range cfg.Layouts {
		all = append(all, layoutNamespaces(layout, cfg.Namespaces)...)
	}
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
				log.Printf("secretbench: repeat %d/%d, layout %s, %d Secrets",
					repeat, cfg.Repeats, layout, count)
				smp, err := b.measure(ctx, layout, repeat, names)
				if err != nil && ctx.Err() != nil {
					// Interrupted mid-step: the partial sample would skew
					// the fit, so the summary keeps the finished steps only.
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
	return nil
}
