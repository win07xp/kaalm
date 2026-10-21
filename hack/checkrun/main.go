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

// checkrun exits 0 only when a named check run passed on a commit. The
// release-on-merge tag job runs it before tagging, so a release PR whose
// upgrade e2e did not pass is neither tagged nor published.
//
// It lists the check runs on HEAD_SHA, keeps the ones GitHub Actions created
// with exactly the name CHECK_NAME, and takes the newest by id: a rerun
// creates a new check run with a higher id, so the latest attempt decides,
// whether it passed or failed. Only the github-actions app counts, so a
// check run another app reports under the same name cannot stand in for
// the workflow's own. That run passes only when it completed with
// conclusion success; a pending, failed, skipped, cancelled, or missing run
// fails the step. A transient API error also fails it, and a rerun is the
// recovery.
//
// Usage (in a workflow step):
//
//	GITHUB_TOKEN=... GITHUB_REPOSITORY=owner/repo HEAD_SHA=<sha> \
//	  CHECK_NAME='e2e-upgrade (from the latest release)' go run ./hack/checkrun
//
// GITHUB_API_URL defaults to https://api.github.com. The program uses only
// the standard library, so `go run` downloads nothing.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const actionsAppSlug = "github-actions"

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

type checkRunList struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []checkRun `json:"check_runs"`
}

func main() {
	os.Exit(run(os.Getenv, http.DefaultClient, os.Stdout, os.Stderr))
}

func run(getenv func(string) string, client *http.Client, stdout, stderr io.Writer) int {
	vals := map[string]string{}
	for _, key := range []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "HEAD_SHA", "CHECK_NAME"} {
		v := getenv(key)
		if v == "" {
			_, _ = fmt.Fprintf(stderr, "::error::%s is not set\n", key)
			return 1
		}
		vals[key] = v
	}
	api := getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	sha, name := vals["HEAD_SHA"], vals["CHECK_NAME"]

	runs, err := listCheckRuns(client, api, vals["GITHUB_REPOSITORY"], sha, name, vals["GITHUB_TOKEN"])
	if err == nil {
		var passed checkRun
		passed, err = evaluate(runs, name, sha)
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "check %q passed on %s: %s\n", name, sha, passed.HTMLURL)
			return 0
		}
	}
	_, _ = fmt.Fprintf(stderr, "::error::%v\n", err)
	return 1
}

func listCheckRuns(client *http.Client, api, repo, sha, name, token string) ([]checkRun, error) {
	u := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?check_name=%s&filter=all&per_page=100",
		strings.TrimRight(api, "/"), repo, url.PathEscape(sha), url.QueryEscape(name))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list check runs on %s: %w", sha, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read check runs on %s: %w", sha, err)
	}
	if resp.StatusCode != http.StatusOK {
		excerpt := strings.Join(strings.Fields(string(body)), " ")
		if len(excerpt) > 200 {
			excerpt = excerpt[:200]
		}
		return nil, fmt.Errorf("list check runs on %s: %s: %s", sha, resp.Status, excerpt)
	}
	var list checkRunList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse check runs on %s: %w", sha, err)
	}
	// Deciding from a partial page could pick an older attempt.
	if list.TotalCount > len(list.CheckRuns) {
		return nil, fmt.Errorf(
			"commit %s has %d check runs named %q, more than one page of %d; not deciding from a partial list",
			sha, list.TotalCount, name, len(list.CheckRuns))
	}
	return list.CheckRuns, nil
}

// evaluate picks the newest GitHub Actions check run named name and returns
// it when it completed with success, or an error that says why not.
func evaluate(runs []checkRun, name, sha string) (checkRun, error) {
	var newest *checkRun
	for i := range runs {
		r := &runs[i]
		if r.App.Slug != actionsAppSlug || r.Name != name {
			continue
		}
		if newest == nil || r.ID > newest.ID {
			newest = r
		}
	}
	switch {
	case newest == nil:
		return checkRun{}, fmt.Errorf("no %q check run on the release PR's head commit %s. "+
			"The upgrade e2e runs on a PR that changes a file under releases/, "+
			"and the release is tagged only after it passes there. Nothing was tagged or published", name, sha)
	case newest.Status != "completed":
		return checkRun{}, fmt.Errorf("check %q on %s has status %s: wait for it to pass, then rerun this job (%s)",
			name, sha, newest.Status, newest.HTMLURL)
	case newest.Conclusion != "success":
		return checkRun{}, fmt.Errorf("check %q on %s concluded %s; nothing was tagged or published (%s)",
			name, sha, newest.Conclusion, newest.HTMLURL)
	}
	return *newest, nil
}
