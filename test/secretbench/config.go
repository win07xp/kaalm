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
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// labelPhase marks every object the benchmark creates, so clearing
	// Secrets never touches anything else.
	labelPhase = "perf.kaalm.io/phase"
	phaseValue = "secretbench"

	// oneNamespace holds every Secret in the one layout; the spread layout
	// uses spreadNamespaceFormat with an index.
	oneNamespace          = "secretbench"
	spreadNamespaceFormat = "secretbench-%02d"

	layoutOne    = "one"
	layoutSpread = "spread"

	// syncTimeout is how long a prototype's read waits for its informer
	// before reading live, the same as secretwatch.Watcher's default
	// SyncTimeout, so "served" means the same thing for every variant.
	syncTimeout = 2 * time.Second
)

// config is one benchmark run's settings.
type config struct {
	Counts        []int
	Namespaces    int
	Layouts       []string
	Repeats       int
	Variants      []string
	Readers       int
	BQPS          float64
	BBurst        int
	UpdateSamples int
	SettleTimeout time.Duration
	Out           string
	RBACCheck     bool
}

func parseFlags(args []string) (config, error) {
	var cfg config
	var counts, layouts, variantNames string
	fs := flag.NewFlagSet("secretbench", flag.ContinueOnError)
	fs.StringVar(&counts, "counts", "0,250,500,1000,2000,5000",
		"Secret counts to step through, ascending; the Secret set grows in place between steps")
	fs.IntVar(&cfg.Namespaces, "namespaces", 20, "namespaces the spread layout round-robins its Secrets over")
	fs.StringVar(&layouts, "layouts", layoutOne+","+layoutSpread,
		"layouts to run: one (every Secret in one namespace) and spread (over -namespaces namespaces)")
	fs.IntVar(&cfg.Repeats, "repeats", 3, "full sweeps to run; the slope fit pools them")
	fs.StringVar(&variantNames, "variants", strings.Join(variantNamesAll(), ","),
		"variants to run at each step, in this order")
	fs.IntVar(&cfg.Readers, "readers", 32, "concurrent reads during a cold start")
	fs.Float64Var(&cfg.BQPS, "b-qps", 500, "client QPS of variant B")
	fs.IntVar(&cfg.BBurst, "b-burst", 1000, "client burst of variant B")
	fs.IntVar(&cfg.UpdateSamples, "update-samples", 10, "Secrets updated one at a time per step to time update delivery")
	fs.DurationVar(&cfg.SettleTimeout, "settle-timeout", 30*time.Minute,
		"how long a step waits for every watch to open before it samples anyway")
	fs.StringVar(&cfg.Out, "out", "", "summary JSON path (default test/secretbench/results/<UTC time>.json)")
	fs.BoolVar(&cfg.RBACCheck, "rbac-check", false,
		"run the RBAC check as a user bound to a resourceNames-scoped Role instead of the sweep; the sweep flags are ignored")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	var err error
	if cfg.Counts, err = parseCounts(counts); err != nil {
		return config{}, err
	}
	cfg.Layouts = splitList(layouts)
	for _, l := range cfg.Layouts {
		if l != layoutOne && l != layoutSpread {
			return config{}, fmt.Errorf("-layouts: unknown layout %q (want %s or %s)", l, layoutOne, layoutSpread)
		}
	}
	if len(cfg.Layouts) == 0 {
		return config{}, errors.New("-layouts: at least one layout is required")
	}
	cfg.Variants = splitList(variantNames)
	if len(cfg.Variants) == 0 {
		return config{}, errors.New("-variants: at least one variant is required")
	}
	known := variantNamesAll()
	for _, v := range cfg.Variants {
		if !slices.Contains(known, v) {
			return config{}, fmt.Errorf("-variants: unknown variant %q (known: %s)", v, strings.Join(known, ", "))
		}
	}
	switch {
	case cfg.Repeats < 1:
		return config{}, errors.New("-repeats must be at least 1")
	case cfg.Namespaces < 1:
		return config{}, errors.New("-namespaces must be at least 1")
	case cfg.Readers < 1:
		return config{}, errors.New("-readers must be at least 1")
	case cfg.UpdateSamples < 0:
		return config{}, errors.New("-update-samples must not be negative")
	case cfg.BQPS <= 0 || cfg.BBurst < 1:
		return config{}, errors.New("-b-qps must be positive and -b-burst at least 1")
	case cfg.SettleTimeout <= 0:
		return config{}, errors.New("-settle-timeout must be positive")
	}
	if cfg.Out == "" && !cfg.RBACCheck {
		cfg.Out = filepath.Join("test", "secretbench", "results",
			time.Now().UTC().Format("20060102-150405")+".json")
	}
	return cfg, nil
}

// parseCounts reads a comma-separated list of non-negative, strictly
// ascending Secret counts.
func parseCounts(s string) ([]int, error) {
	var out []int
	for _, part := range splitList(s) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("-counts: %q is not a number", part)
		}
		if n < 0 {
			return nil, fmt.Errorf("-counts: %d is negative", n)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return nil, fmt.Errorf("-counts: %d does not follow %d; counts must ascend", n, out[len(out)-1])
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("-counts: at least one count is required")
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
