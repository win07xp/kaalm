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

package controller

import (
	"slices"
	"testing"
)

// Rule 51: invalidNamespacePatterns names each malformed allowedNamespaces
// entry once, and never a valid one.
func TestInvalidNamespacePatterns(t *testing.T) {
	msg := func(p string) string { return `allowedNamespaces entry "` + p + `" is not a valid glob pattern` }
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"valid entries", []string{"*", "team-*", "exact", "team-[a-z]*", "[!a]", `\*`, ""}, nil},
		{"unclosed class", []string{"["}, []string{msg("[")}},
		{"unclosed class after a prefix", []string{"team-["}, []string{msg("team-[")}},
		{"trailing escape", []string{`a\`}, []string{`allowedNamespaces entry "a\\" is not a valid glob pattern`}},
		{"empty class", []string{"[]"}, []string{msg("[]")}},
		{"open range", []string{"[a-]"}, []string{msg("[a-]")}},
		{"repeat reported once, valid entry skipped", []string{"team-a", "[", "["}, []string{msg("[")}},
		{"spec order kept", []string{"team-[", "ok", "["}, []string{msg("team-["), msg("[")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := invalidNamespacePatterns(tc.patterns); !slices.Equal(got, tc.want) {
				t.Errorf("invalidNamespacePatterns(%q) = %q, want %q", tc.patterns, got, tc.want)
			}
		})
	}
}
