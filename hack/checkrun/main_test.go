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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testName = "e2e-upgrade (from the latest release)"

func cr(id int64, name, slug, status, conclusion string) checkRun {
	c := checkRun{ID: id, Name: name, Status: status, Conclusion: conclusion,
		HTMLURL: "https://example.test/runs/x"}
	c.App.Slug = slug
	return c
}

func TestEvaluate(t *testing.T) {
	const gha = "github-actions"
	cases := []struct {
		name    string
		runs    []checkRun
		wantErr string // empty means pass
	}{
		{"newest succeeded", []checkRun{cr(1, testName, gha, "completed", "success")}, ""},
		{"no runs", nil, "releases/"},
		{"newest in progress", []checkRun{cr(1, testName, gha, "in_progress", "")}, "status in_progress"},
		{"newest queued", []checkRun{cr(1, testName, gha, "queued", "")}, "status queued"},
		{"success then failure", []checkRun{
			cr(1, testName, gha, "completed", "success"),
			cr(2, testName, gha, "completed", "failure"),
		}, "concluded failure"},
		{"failure then success", []checkRun{
			cr(2, testName, gha, "completed", "success"),
			cr(1, testName, gha, "completed", "failure"),
		}, ""},
		{"skipped", []checkRun{cr(1, testName, gha, "completed", "skipped")}, "concluded skipped"},
		{"cancelled", []checkRun{cr(1, testName, gha, "completed", "cancelled")}, "concluded cancelled"},
		{"neutral", []checkRun{cr(1, testName, gha, "completed", "neutral")}, "concluded neutral"},
		{"timed out", []checkRun{cr(1, testName, gha, "completed", "timed_out")}, "concluded timed_out"},
		{"other app success, actions failed", []checkRun{
			cr(5, testName, "other-app", "completed", "success"),
			cr(1, testName, gha, "completed", "failure"),
		}, "concluded failure"},
		{"only another app", []checkRun{cr(5, testName, "other-app", "completed", "success")}, "releases/"},
		{"prefixed name ignored", []checkRun{
			cr(9, "upgrade / "+testName, gha, "completed", "success"),
		}, "releases/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evaluate(tc.runs, testName, "abc123")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func env(api string) func(string) string {
	m := map[string]string{
		"GITHUB_TOKEN":      "tok",
		"GITHUB_REPOSITORY": "o/r",
		"HEAD_SHA":          "SHA",
		"CHECK_NAME":        testName,
		"GITHUB_API_URL":    api,
	}
	return func(k string) string { return m[k] }
}

func serve(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/commits/SHA/check-runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("check_name") != testName || q.Get("filter") != "all" || q.Get("per_page") != "100" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func listing(runs ...checkRun) map[string]any {
	return map[string]any{"total_count": len(runs), "check_runs": runs}
}

func TestRunPasses(t *testing.T) {
	r := cr(7, testName, "github-actions", "completed", "success")
	r.HTMLURL = "https://example.test/run/7"
	srv := serve(t, http.StatusOK, listing(r))
	var out, errOut bytes.Buffer
	if code := run(env(srv.URL), srv.Client(), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "https://example.test/run/7") {
		t.Errorf("stdout %q lacks the run URL", out.String())
	}
}

func TestRunRefusesFailedCheck(t *testing.T) {
	srv := serve(t, http.StatusOK, listing(cr(7, testName, "github-actions", "completed", "failure")))
	var out, errOut bytes.Buffer
	if code := run(env(srv.URL), srv.Client(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.HasPrefix(errOut.String(), "::error::") {
		t.Errorf("stderr %q does not start with ::error::", errOut.String())
	}
}

func TestRunAPIErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		srv := serve(t, status, map[string]string{"message": "nope"})
		var out, errOut bytes.Buffer
		if code := run(env(srv.URL), srv.Client(), &out, &errOut); code != 1 {
			t.Fatalf("status %d: exit %d, want 1", status, code)
		}
		if !strings.Contains(errOut.String(), http.StatusText(status)) {
			t.Errorf("status %d: stderr %q lacks the status", status, errOut.String())
		}
	}
}

func TestRunRefusesPartialPage(t *testing.T) {
	body := listing(cr(7, testName, "github-actions", "completed", "success"))
	body["total_count"] = 101
	srv := serve(t, http.StatusOK, body)
	var out, errOut bytes.Buffer
	if code := run(env(srv.URL), srv.Client(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestRunMissingEnv(t *testing.T) {
	for _, key := range []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "HEAD_SHA", "CHECK_NAME"} {
		requests := 0
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
		base := env(srv.URL)
		getenv := func(k string) string {
			if k == key {
				return ""
			}
			return base(k)
		}
		var out, errOut bytes.Buffer
		if code := run(getenv, srv.Client(), &out, &errOut); code != 1 {
			t.Errorf("%s unset: exit %d, want 1", key, code)
		}
		if !strings.Contains(errOut.String(), key) {
			t.Errorf("%s unset: stderr %q does not name it", key, errOut.String())
		}
		srv.Close()
		if requests != 0 {
			t.Errorf("%s unset: made %d requests", key, requests)
		}
	}
}
