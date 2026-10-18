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
)

// The helpers here find malformed path.Match patterns in the glob lists the
// reconcilers report on Ready: allowedNamespaces (rule 51) and allowedImages
// (rule 52). Matching itself is unchanged for both fields: a malformed entry
// still matches nothing, and the valid entries still admit their namespaces or
// images. The report is bounded as entryList bounds every per-entry list.

// invalidGlobPatterns returns one message for each of the first
// maxListedEntries distinct malformed entries of the named field, in spec
// order, and one more message counting the rest. path.Match reports
// ErrBadPattern for a malformed pattern whatever the name, so matching
// against "" finds them all. The count message reads "<field> has", which
// sorts after the field's "<field> entry" messages.
func invalidGlobPatterns(field string, patterns []string) []string {
	var bad entryList
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err == nil {
			continue
		}
		bad.add(p, fmt.Sprintf("%s entry %s is not a valid glob pattern", field, quotedEntry(p)))
	}
	return bad.messages(
		fmt.Sprintf("%s has 1 more malformed entry, not listed", field),
		field+" has %d more malformed entries, not listed")
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
