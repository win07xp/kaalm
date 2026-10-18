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
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
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
		{"long entry quoted as its first 256 bytes", []string{"[" + strings.Repeat("a", 40<<10)},
			[]string{`allowedNamespaces entry "[` + strings.Repeat("a", 255) +
				`" (first 256 of 40961 bytes) is not a valid glob pattern`}},
		{"entry of exactly 256 bytes quoted whole", []string{"[" + strings.Repeat("a", 255)},
			[]string{msg("[" + strings.Repeat("a", 255))}},
		{"cut steps back to a rune start", []string{"[" + strings.Repeat("a", 254) + "\u20ac" + strings.Repeat("b", 10)},
			[]string{`allowedNamespaces entry "[` + strings.Repeat("a", 254) +
				`" (first 255 of 268 bytes) is not a valid glob pattern`}},
		{"ten entries listed, no count", numbered(10), firstTenMsgs("allowedNamespaces")},
		{"eleventh entry counted", numbered(11),
			append(firstTenMsgs("allowedNamespaces"), "allowedNamespaces has 1 more malformed entry, not listed")},
		{"repeats and valid entries not counted",
			append([]string{"ok", "bad0["}, append(numbered(12), "bad0[", "team-*", "bad11[")...),
			append(firstTenMsgs("allowedNamespaces"), "allowedNamespaces has 2 more malformed entries, not listed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := invalidNamespacePatterns(tc.patterns); !slices.Equal(got, tc.want) {
				t.Errorf("invalidNamespacePatterns(%q) = %q, want %q", tc.patterns, got, tc.want)
			}
		})
	}
}

// Rule 52: invalidImagePatterns names each malformed allowedImages entry
// once, and never a valid one.
func TestInvalidImagePatterns(t *testing.T) {
	msg := func(p string) string { return `allowedImages entry "` + p + `" is not a valid glob pattern` }
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"valid entries", []string{"registry.test/agents/*", "ghcr.io/win07xp/*:v*", "docker.io/library/nginx", "[!a]*"}, nil},
		{"unclosed class", []string{"["}, []string{msg("[")}},
		{"unclosed class after a prefix", []string{"registry.test/["}, []string{msg("registry.test/[")}},
		{"trailing escape", []string{`a\`}, []string{`allowedImages entry "a\\" is not a valid glob pattern`}},
		{"repeat reported once, valid entry skipped", []string{"ok/*", "[", "["}, []string{msg("[")}},
		{"spec order kept", []string{"registry.test/[", "ok", "["}, []string{msg("registry.test/["), msg("[")}},
		{"long entry quoted as its first 256 bytes", []string{"[" + strings.Repeat("a", 40<<10)},
			[]string{`allowedImages entry "[` + strings.Repeat("a", 255) +
				`" (first 256 of 40961 bytes) is not a valid glob pattern`}},
		{"eleventh entry counted", numbered(11),
			append(firstTenMsgs("allowedImages"), "allowedImages has 1 more malformed entry, not listed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := invalidImagePatterns(tc.patterns); !slices.Equal(got, tc.want) {
				t.Errorf("invalidImagePatterns(%q) = %q, want %q", tc.patterns, got, tc.want)
			}
		})
	}
}

// numbered returns n distinct malformed patterns, bad0[ to bad<n-1>[.
func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "bad" + strconv.Itoa(i) + "["
	}
	return out
}

// firstTenMsgs is the message invalidGlobPatterns gives for each of the
// first ten entries of numbered.
func firstTenMsgs(field string) []string {
	out := make([]string, 10)
	for i := range out {
		out[i] = fmt.Sprintf("%s entry %q is not a valid glob pattern", field, "bad"+strconv.Itoa(i)+"[")
	}
	return out
}

// conditionMessageLimit is the condition schema's maxLength for
// status.conditions[].message. A longer message fails the whole status write.
const conditionMessageLimit = 32768

// The worst case for the AgentClass Ready message: both fields hold many long
// malformed entries made of control bytes, which %q writes as four bytes
// each. Joined as the reconciler joins them, the list stays under the limit.
func TestInvalidGlobPatternsWorstCaseFitsConditionMessage(t *testing.T) {
	var patterns []string
	for i := range 50 {
		patterns = append(patterns, "["+strings.Repeat("\x00", 1000)+strconv.Itoa(i))
	}
	problems := append(invalidNamespacePatterns(patterns), invalidImagePatterns(patterns)...)
	sort.Strings(problems)
	msg := strings.Join(problems, "; ")
	if len(msg) >= conditionMessageLimit {
		t.Fatalf("worst-case message is %d bytes, want under %d", len(msg), conditionMessageLimit)
	}
	for _, want := range []string{
		"allowedNamespaces has 40 more malformed entries, not listed",
		"allowedImages has 40 more malformed entries, not listed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("worst-case message does not hold %q", want)
		}
	}
}
