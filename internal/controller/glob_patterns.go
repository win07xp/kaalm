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
	"path"
	"unicode/utf8"
)

// The helpers here find malformed path.Match patterns in the glob lists the
// reconcilers report on Ready: allowedNamespaces (rule 51) and allowedImages
// (rule 52). Matching itself is unchanged for both fields: a malformed entry
// still matches nothing, and the valid entries still admit their namespaces or
// images. The report lists up to maxListedPatterns entries, each quoted up to
// maxQuotedPatternBytes, and counts the rest.

// The report goes into a Ready condition message, which the condition schema
// caps at 32768 characters. A longer message fails the whole status write and
// leaves Ready at its last value. In the worst case (%q writes a control byte
// as four bytes) one field's report is about 11 KiB, so an AgentClass's two
// fields stay near 22 KiB and leave room for the class's other problems.
const (
	maxQuotedPatternBytes = 256
	maxListedPatterns     = 10
)

// quotedPattern quotes p, or only its first maxQuotedPatternBytes bytes and
// its full length when it is longer. The cut steps back to a rune start so
// the quote never ends in a split rune.
func quotedPattern(p string) string {
	if len(p) <= maxQuotedPatternBytes {
		return fmt.Sprintf("%q", p)
	}
	cut := maxQuotedPatternBytes
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(p[cut]); back++ {
		cut--
	}
	if !utf8.RuneStart(p[cut]) {
		// Not valid UTF-8 here: %q escapes the bytes, so any cut reads right.
		cut = maxQuotedPatternBytes
	}
	return fmt.Sprintf("%q (first %d of %d bytes)", p[:cut], cut, len(p))
}

// invalidGlobPatterns returns one message for each of the first
// maxListedPatterns distinct malformed entries of the named field, in spec
// order, and one more message counting the rest. path.Match reports
// ErrBadPattern for a malformed pattern whatever the name, so matching
// against "" finds them all. The count message reads "<field> has", which
// sorts after the field's "<field> entry" messages.
func invalidGlobPatterns(field string, patterns []string) []string {
	var bad []string
	more := 0
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := path.Match(p, ""); err == nil {
			continue
		}
		if len(bad) < maxListedPatterns {
			bad = append(bad, fmt.Sprintf("%s entry %s is not a valid glob pattern", field, quotedPattern(p)))
		} else {
			more++
		}
	}
	switch {
	case more == 1:
		bad = append(bad, fmt.Sprintf("%s has 1 more malformed entry, not listed", field))
	case more > 1:
		bad = append(bad, fmt.Sprintf("%s has %d more malformed entries, not listed", field, more))
	}
	return bad
}

// invalidNamespacePatterns checks allowedNamespaces (rule 51). The AgentClass,
// ModelProvider, and ToolProvider reconcilers report the result on Ready.
func invalidNamespacePatterns(patterns []string) []string {
	return invalidGlobPatterns("allowedNamespaces", patterns)
}

// invalidImagePatterns checks an AgentClass's image.allowedImages (rule 52).
// The AgentClassReconciler reports the result on Ready.
func invalidImagePatterns(patterns []string) []string {
	return invalidGlobPatterns("allowedImages", patterns)
}
