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
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// These tests keep the release workflows and this program in step: the
// release-on-merge tag job finds the upgrade e2e's check run by its job name,
// so a rename in upgrade.yml must also change CHECK_NAME in release-merge.yml.

const (
	upgradeWorkflow      = "../../.github/workflows/upgrade.yml"
	releaseMergeWorkflow = "../../.github/workflows/release-merge.yml"
	upgradeJobID         = "e2e-upgrade"
	tagStepName          = "Tag the merge commit"
	headSHAExpr          = "${{ github.event.pull_request.head.sha }}"
)

func loadWorkflow(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf map[string]any
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

func mapAt(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("no mapping at %q (path %v)", k, keys)
		}
		cur = next
	}
	return cur
}

func TestUpgradeWorkflowRunsOnReleasePRs(t *testing.T) {
	wf := loadWorkflow(t, upgradeWorkflow)
	// sigs.k8s.io/yaml reads the bare YAML 1.1 key `on` as the boolean true,
	// so the triggers sit under the string key "true".
	pr := mapAt(t, wf, "true", "pull_request")
	paths, _ := pr["paths"].([]any)
	for _, p := range paths {
		if p == "releases/**" {
			return
		}
	}
	t.Fatalf("upgrade.yml pull_request paths %v lack releases/**, so a release PR would not run the upgrade e2e", paths)
}

func TestReleaseMergeHasNoPostMergeUpgradeJob(t *testing.T) {
	wf := loadWorkflow(t, releaseMergeWorkflow)
	for id, job := range mapAt(t, wf, "jobs") {
		j, _ := job.(map[string]any)
		if uses, _ := j["uses"].(string); strings.HasSuffix(uses, "upgrade.yml") {
			t.Errorf("release-merge.yml job %q still runs the upgrade e2e after the merge", id)
		}
	}
}

func TestTagJobChecksTheUpgradeRunBeforeTagging(t *testing.T) {
	upgrade := loadWorkflow(t, upgradeWorkflow)
	wantName, _ := mapAt(t, upgrade, "jobs", upgradeJobID)["name"].(string)
	if wantName == "" {
		t.Fatalf("upgrade.yml job %s has no name", upgradeJobID)
	}

	merge := loadWorkflow(t, releaseMergeWorkflow)
	tag := mapAt(t, merge, "jobs", "tag")

	perms := mapAt(t, tag, "permissions")
	if perms["checks"] != "read" {
		t.Errorf("tag job permissions.checks = %v, want read", perms["checks"])
	}
	if perms["contents"] != "write" {
		t.Errorf("tag job permissions.contents = %v, want write", perms["contents"])
	}

	steps, _ := tag["steps"].([]any)
	checkIdx, tagIdx := -1, -1
	for i, s := range steps {
		step, _ := s.(map[string]any)
		if run, _ := step["run"].(string); strings.Contains(run, "go run ./hack/checkrun") {
			checkIdx = i
			env, _ := step["env"].(map[string]any)
			if env["CHECK_NAME"] != wantName {
				t.Errorf("check step CHECK_NAME = %v, want upgrade.yml's job name %q", env["CHECK_NAME"], wantName)
			}
			if env["HEAD_SHA"] != headSHAExpr {
				t.Errorf("check step HEAD_SHA = %v, want %s", env["HEAD_SHA"], headSHAExpr)
			}
		}
		if step["name"] == tagStepName {
			tagIdx = i
		}
	}
	switch {
	case checkIdx < 0:
		t.Errorf("tag job has no step that runs go run ./hack/checkrun")
	case tagIdx < 0:
		t.Errorf("tag job has no step named %q", tagStepName)
	case checkIdx > tagIdx:
		t.Errorf("the check step (index %d) runs after %q (index %d)", checkIdx, tagStepName, tagIdx)
	}
}
