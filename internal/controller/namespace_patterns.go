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

// invalidNamespacePatterns returns one message for each distinct malformed
// allowedNamespaces entry, in spec order (rule 51). path.Match reports
// ErrBadPattern for a malformed pattern whatever the name, so matching
// against "" finds them all. The AgentClass, ModelProvider, and ToolProvider
// reconcilers report the result on Ready. Matching itself is unchanged: a
// malformed entry still matches nothing, and the valid entries still admit
// their namespaces.
func invalidNamespacePatterns(patterns []string) []string {
	var bad []string
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := path.Match(p, ""); err != nil {
			bad = append(bad, fmt.Sprintf("allowedNamespaces entry %q is not a valid glob pattern", p))
		}
	}
	return bad
}
