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

// Package dashboards pins the shipped Grafana dashboards (config/grafana) to
// the metric catalog in docs/src/operations/observability.md: every metric a
// panel queries must be in the catalog, every catalog metric must be on some
// panel, and the files must import with no manual edits (a datasource
// template variable, no __inputs).
package dashboards

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type dashboard struct {
	UID           string   `json:"uid"`
	Title         string   `json:"title"`
	Tags          []string `json:"tags"`
	SchemaVersion int      `json:"schemaVersion"`
	Inputs        []any    `json:"__inputs"`
	Templating    struct {
		List []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"list"`
	} `json:"templating"`
	Panels []panel `json:"panels"`
}

type panel struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Datasource struct {
		UID string `json:"uid"`
	} `json:"datasource"`
	Targets []struct {
		Expr       string `json:"expr"`
		Datasource struct {
			UID string `json:"uid"`
		} `json:"datasource"`
	} `json:"targets"`
	Panels []panel `json:"panels"` // collapsed rows nest their panels
}

func repo(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()
	files, err := filepath.Glob(repo("config", "grafana", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards under config/grafana: %v", err)
	}
	out := map[string]dashboard{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d dashboard
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(f)] = d
	}
	return out
}

// catalog returns every metric name in the observability page's aggregated
// catalog table, regardless of source.
func catalog(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(repo("docs", "src", "operations", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[2]), "`")
		if strings.HasPrefix(name, "kaalm_") {
			names[name] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("the catalog table parsed to nothing")
	}
	return names
}

func flatten(ps []panel) []panel {
	var out []panel
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatten(p.Panels)...)
	}
	return out
}

var (
	metricToken = regexp.MustCompile(`\bkaalm_[a-z0-9_]+`)
	varToken    = regexp.MustCompile(`\$(__)?[a-zA-Z_]+`)
	// histogram series carry one of these suffixes on the wire
	histogramSuffix = regexp.MustCompile(`_(bucket|sum|count)$`)
)

// baseName maps a series name on the wire back to its catalog name.
func baseName(token string) string {
	return histogramSuffix.ReplaceAllString(token, "")
}

func TestDashboards_QueryOnlyCatalogMetrics(t *testing.T) {
	cat := catalog(t)
	for file, d := range loadDashboards(t) {
		for _, p := range flatten(d.Panels) {
			for _, tg := range p.Targets {
				for _, tok := range metricToken.FindAllString(tg.Expr, -1) {
					if !cat[baseName(tok)] {
						t.Errorf("%s panel %q queries %s, which is not in the catalog", file, p.Title, tok)
					}
				}
			}
		}
	}
}

func TestDashboards_CoverTheWholeCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range loadDashboards(t) {
		for _, p := range flatten(d.Panels) {
			for _, tg := range p.Targets {
				for _, tok := range metricToken.FindAllString(tg.Expr, -1) {
					seen[baseName(tok)] = true
				}
			}
		}
	}
	var missing []string
	for name := range catalog(t) {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("catalog metrics on no dashboard: %v", missing)
	}
}

func TestDashboards_ImportWithNoManualEdits(t *testing.T) {
	builtin := map[string]bool{"$__rate_interval": true, "$__range": true, "$__interval": true}
	uids := map[string]string{}
	for file, d := range loadDashboards(t) {
		if want := strings.TrimSuffix(file, ".json"); d.UID != want {
			t.Errorf("%s: uid %q, want the file name %q", file, d.UID, want)
		}
		if prev, dup := uids[d.UID]; dup {
			t.Errorf("%s and %s share uid %q", file, prev, d.UID)
		}
		uids[d.UID] = file
		if !strings.HasPrefix(d.Title, "Kaalm / ") {
			t.Errorf("%s: title %q should start with \"Kaalm / \"", file, d.Title)
		}
		if len(d.Inputs) != 0 {
			t.Errorf("%s: __inputs present; file provisioning never substitutes them", file)
		}
		if d.SchemaVersion < 39 {
			t.Errorf("%s: schemaVersion %d, want 39 or later", file, d.SchemaVersion)
		}
		tagged := false
		for _, tag := range d.Tags {
			tagged = tagged || tag == "kaalm"
		}
		if !tagged {
			t.Errorf("%s: missing the kaalm tag the dashboard links use", file)
		}
		vars := map[string]bool{}
		for _, v := range d.Templating.List {
			vars["$"+v.Name] = true
			if v.Name == "datasource" && v.Type != "datasource" {
				t.Errorf("%s: the datasource variable must be of type datasource", file)
			}
		}
		if !vars["$datasource"] {
			t.Errorf("%s: no datasource template variable", file)
		}
		ids := map[int]bool{}
		for _, p := range flatten(d.Panels) {
			if ids[p.ID] {
				t.Errorf("%s: duplicate panel id %d", file, p.ID)
			}
			ids[p.ID] = true
			if p.Type == "row" {
				continue
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s panel %q has no query", file, p.Title)
			}
			if p.Datasource.UID != "${datasource}" {
				t.Errorf("%s panel %q: datasource %q, want ${datasource}", file, p.Title, p.Datasource.UID)
			}
			for _, tg := range p.Targets {
				if tg.Datasource.UID != "${datasource}" {
					t.Errorf("%s panel %q: target datasource %q, want ${datasource}", file, p.Title, tg.Datasource.UID)
				}
				for _, v := range varToken.FindAllString(tg.Expr, -1) {
					if !vars[v] && !builtin[v] {
						t.Errorf("%s panel %q references %s, which is neither a template variable nor a Grafana builtin",
							file, p.Title, v)
					}
				}
			}
		}
	}
}
