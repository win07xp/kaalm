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
	"testing"
)

func TestParseStatReadsCommWithParentheses(t *testing.T) {
	s, err := parseStat("4242 (my (odd) cmd) S 17 4242 4242 0 -1 4194560 100 0 0 0")
	if err != nil {
		t.Fatal(err)
	}
	if s.PID != 4242 || s.PPID != 17 || s.Comm != "my (odd) cmd" {
		t.Fatalf("parseStat = %+v, want PID 4242, PPID 17, comm %q", s, "my (odd) cmd")
	}
	if _, err := parseStat("garbage"); err == nil {
		t.Fatal("parseStat accepted a line without a command name")
	}
}

func TestChildrenByCommPicksThisProcessChildren(t *testing.T) {
	stats := []procStat{
		{PID: 10, PPID: 1, Comm: "etcd"},
		{PID: 11, PPID: 5, Comm: "etcd"},
		{PID: 12, PPID: 5, Comm: "kube-apiserver"},
		{PID: 13, PPID: 12, Comm: "child"},
	}
	got := childrenByComm(stats, 5)
	if got["etcd"] != 11 || got["kube-apiserver"] != 12 || len(got) != 2 {
		t.Fatalf("childrenByComm = %v, want etcd 11 and kube-apiserver 12", got)
	}
}

func TestParseVmRSS(t *testing.T) {
	status := "Name:\tetcd\nVmPeak:\t  900 kB\nVmRSS:\t   51200 kB\nThreads:\t12\n"
	got, err := parseVmRSS(status)
	if err != nil {
		t.Fatal(err)
	}
	if got != 51200*1024 {
		t.Fatalf("parseVmRSS = %d, want %d", got, 51200*1024)
	}
	if _, err := parseVmRSS("Name:\tkthreadd\n"); err == nil {
		t.Fatal("parseVmRSS accepted a status without VmRSS")
	}
}

func TestSecretWatchesSumsScopes(t *testing.T) {
	text := `# HELP apiserver_longrunning_requests [STABLE] Gauge of all active long-running apiserver requests.
# TYPE apiserver_longrunning_requests gauge
apiserver_longrunning_requests{resource="secrets",scope="namespace",verb="WATCH"} 7
apiserver_longrunning_requests{resource="secrets",scope="cluster",verb="WATCH"} 2
apiserver_longrunning_requests{resource="configmaps",scope="namespace",verb="WATCH"} 40
apiserver_longrunning_requests{resource="pods",scope="namespace",verb="CONNECT"} 1
`
	got, ok, err := secretWatches([]byte(text))
	if err != nil || !ok || got != 9 {
		t.Fatalf("secretWatches = %v, %v, %v; want 9, true, nil", got, ok, err)
	}
	if _, ok, err := secretWatches([]byte("# TYPE other gauge\nother 1\n")); ok || err != nil {
		t.Fatalf("secretWatches on text without the family = ok %v, err %v; want false, nil", ok, err)
	}
}

// Text the parser rejects must fail the read, not read as zero watches:
// a zero would also let the drain check after Stop pass at once.
func TestSecretWatchesFailsOnUnparsableText(t *testing.T) {
	text := "apiserver_longrunning_requests{resource=\"secrets\",verb=\"WATCH\" 3\n"
	if n, ok, err := secretWatches([]byte(text)); err == nil {
		t.Fatalf("secretWatches = %v, %v, nil; want a parse error", n, ok)
	}
}
