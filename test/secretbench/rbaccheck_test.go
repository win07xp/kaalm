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
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRBACCheckPassesForBothReaders(t *testing.T) {
	requireEnvtest(t)
	var buf bytes.Buffer
	if err := rbacCheck(context.Background(), testEnv, restCfg, &buf); err != nil {
		t.Fatalf("rbacCheck: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	if strings.Contains(out, "FAIL") {
		t.Fatalf("a check failed:\n%s", out)
	}
	for _, reader := range []string{"today-gw", "lean"} {
		for _, check := range []string{"(a)", "(c)", "(d)"} {
			if !hasPassLine(out, reader, check) {
				t.Errorf("no PASS line for %s %s", reader, check)
			}
		}
	}
	if !hasPassLine(out, "user", "(b)") || !hasPassLine(out, "user", "(c)") {
		t.Errorf("no PASS lines for the user's (b) and (c)")
	}
}

func hasPassLine(out, reader, check string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == reader && f[2] == check && strings.Contains(line, ": PASS") {
			return true
		}
	}
	return false
}

func TestSummarizeFailsOnAnyFailedCheck(t *testing.T) {
	if err := summarize([]checkResult{{Pass: true}, {Pass: true}}); err != nil {
		t.Fatalf("all passed: %v", err)
	}
	err := summarize([]checkResult{{Pass: true}, {Pass: false}})
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("one failed: %v, want an error naming 1 of 2", err)
	}
	var buf bytes.Buffer
	printChecks(&buf, []checkResult{{Reader: "lean", Check: "(d) x", Pass: false, Detail: "d"}})
	if got := buf.String(); !strings.Contains(got, "lean   (d) x: FAIL  d") {
		t.Fatalf("line = %q", got)
	}
}
