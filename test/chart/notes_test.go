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

package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderNotes returns the chart's rendered NOTES.txt. helm template never
// prints NOTES.txt, so this renders a copy of the chart in which the notes
// are wrapped in a named template and emitted as a YAML string.
func renderNotes(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join(t.TempDir(), "kaalm")
	if err := os.CopyFS(chart, os.DirFS(filepath.Join("..", "..", "charts", "kaalm"))); err != nil {
		t.Fatal(err)
	}
	notesPath := filepath.Join(chart, "templates", "NOTES.txt")
	notes, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(notesPath); err != nil {
		t.Fatal(err)
	}
	wrapped := `{{- define "kaalmtest.notes" }}` + string(notes) + `{{- end }}`
	if err := os.WriteFile(filepath.Join(chart, "templates", "_kaalmtest_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	render := `notes: {{ include "kaalmtest.notes" . | toJson }}` + "\n"
	if err := os.WriteFile(filepath.Join(chart, "templates", "kaalmtest-notes.yaml"), []byte(render), 0o600); err != nil {
		t.Fatal(err)
	}
	full := append([]string{
		"template", "kaalm", chart, "-n", releaseNamespace, "-s", "templates/kaalmtest-notes.yaml",
	}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var doc struct {
		Notes string `json:"notes"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse rendered notes: %v\n%s", err, out)
	}
	return doc.Notes
}

// NOTES.txt warns about the deprecated controller probe trust values only when
// one is set, and names the gateway value that replaces it.
func TestNotes_ProbeCADeprecationWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		want     []string
		dontWant []string
	}{
		{
			name:     "defaults",
			dontWant: []string{"deprecated"},
		},
		{
			name: "gateway values only",
			args: []string{
				"--set", "gateway.trustClusterCAForUpstream=true",
				"--set", "gateway.upstreamCA.configMap=corp-ca",
			},
			dontWant: []string{"deprecated"},
		},
		{
			name:     "trustClusterCAForProbes",
			args:     []string{"--set", "controller.trustClusterCAForProbes=true"},
			want:     []string{"controller.trustClusterCAForProbes is deprecated", "gateway.trustClusterCAForUpstream"},
			dontWant: []string{"controller.probeCA is deprecated"},
		},
		{
			name:     "probeCA",
			args:     []string{"--set", "controller.probeCA.configMap=old-ca"},
			want:     []string{"controller.probeCA is deprecated", "gateway.upstreamCA"},
			dontWant: []string{"controller.trustClusterCAForProbes is deprecated"},
		},
		{
			name: "both",
			args: []string{
				"--set", "controller.trustClusterCAForProbes=true",
				"--set", "controller.probeCA.configMap=old-ca",
			},
			want: []string{"controller.trustClusterCAForProbes is deprecated", "controller.probeCA is deprecated"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notes := renderNotes(t, tc.args...)
			for _, w := range tc.want {
				if !strings.Contains(notes, w) {
					t.Errorf("NOTES lack %q:\n%s", w, notes)
				}
			}
			for _, w := range tc.dontWant {
				if strings.Contains(notes, w) {
					t.Errorf("NOTES contain %q:\n%s", w, notes)
				}
			}
		})
	}
}
