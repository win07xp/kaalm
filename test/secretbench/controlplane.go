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
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/kubernetes"
)

// procStat is the part of /proc/<pid>/stat the benchmark reads.
type procStat struct {
	PID, PPID int
	Comm      string
}

// parseStat reads a /proc/<pid>/stat line. The command name sits between the
// first '(' and the last ')', and may itself hold spaces and parentheses.
func parseStat(line string) (procStat, error) {
	open := strings.IndexByte(line, '(')
	closing := strings.LastIndexByte(line, ')')
	if open < 0 || closing < open {
		return procStat{}, fmt.Errorf("stat line without a command name: %q", line)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return procStat{}, fmt.Errorf("stat pid: %w", err)
	}
	rest := strings.Fields(line[closing+1:])
	// rest[0] is the state, rest[1] the parent PID.
	if len(rest) < 2 {
		return procStat{}, fmt.Errorf("stat line too short: %q", line)
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return procStat{}, fmt.Errorf("stat ppid: %w", err)
	}
	return procStat{PID: pid, PPID: ppid, Comm: line[open+1 : closing]}, nil
}

// childrenByComm maps the command name of each direct child of parent to
// its PID.
func childrenByComm(stats []procStat, parent int) map[string]int {
	out := map[string]int{}
	for _, s := range stats {
		if s.PPID == parent {
			out[s.Comm] = s.PID
		}
	}
	return out
}

// controlPlanePIDs are the envtest kube-apiserver and etcd processes; 0 when
// not found.
type controlPlanePIDs struct {
	APIServer, Etcd int
}

// findControlPlane finds the kube-apiserver and etcd that envtest started as
// children of this process. envtest keeps their exec.Cmd private, so the
// PIDs come from /proc.
func findControlPlane() (controlPlanePIDs, error) {
	paths, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return controlPlanePIDs{}, err
	}
	var stats []procStat
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // the process ended
		}
		if s, err := parseStat(string(b)); err == nil {
			stats = append(stats, s)
		}
	}
	children := childrenByComm(stats, os.Getpid())
	pids := controlPlanePIDs{APIServer: children["kube-apiserver"], Etcd: children["etcd"]}
	if pids.APIServer == 0 && pids.Etcd == 0 {
		return pids, errors.New("no kube-apiserver or etcd child process found in /proc")
	}
	return pids, nil
}

// parseVmRSS reads the resident set size in bytes from /proc/<pid>/status
// text.
func parseVmRSS(status string) (int64, error) {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("unexpected VmRSS line %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("no VmRSS line")
}

func rssBytes(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	return parseVmRSS(string(b))
}

// secretWatches sums the API server's open Secret watches over every scope
// from its Prometheus text: apiserver_longrunning_requests with verb WATCH
// and resource secrets. ok is false when the text has no such family; err is
// set when the parser rejects the text, so a failed parse never reads as
// zero watches.
func secretWatches(metrics []byte) (float64, bool, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(metrics)))
	if err != nil {
		return 0, false, fmt.Errorf("parse API server metrics: %w", err)
	}
	mf, ok := families["apiserver_longrunning_requests"]
	if !ok {
		return 0, false, nil
	}
	var sum float64
	for _, m := range mf.Metric {
		if label(m, "verb") == "WATCH" && label(m, "resource") == secretsResource {
			sum += m.GetGauge().GetValue()
		}
	}
	return sum, true, nil
}

func label(m *dto.Metric, name string) string {
	for _, lp := range m.Label {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

func scrapeAPIServer(ctx context.Context, admin kubernetes.Interface) ([]byte, error) {
	return admin.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
}

// apiServerSecretWatches scrapes the API server and returns its open Secret
// watches.
func apiServerSecretWatches(ctx context.Context, admin kubernetes.Interface) (float64, error) {
	body, err := scrapeAPIServer(ctx, admin)
	if err != nil {
		return 0, err
	}
	n, ok, err := secretWatches(body)
	if err != nil {
		return 0, err
	}
	if !ok {
		// No Secret watch has been served yet, so the family has no rows.
		return 0, nil
	}
	return n, nil
}
