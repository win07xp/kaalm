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

package v1beta1

import "path"

// AdmitsNamespace reports whether the class admits Agents and AgentTasks in
// namespace ns (rule 47). An unset allowedNamespaces admits every namespace.
// Otherwise ns must match one of the path.Match glob patterns; a malformed
// pattern matches nothing, and the AgentClassReconciler reports it on Ready
// (rule 51). This is the opposite of the ModelProvider and
// ToolProvider allowedNamespaces checks (namespaceAllowed in the controller,
// namespaceGlobAllowed in the gateway), where an empty list admits none. The
// API server rejects an explicit empty list on the class, so here an empty
// slice can only mean the field is unset.
func (c *AgentClass) AdmitsNamespace(ns string) bool {
	if len(c.Spec.AllowedNamespaces) == 0 {
		return true
	}
	for _, pattern := range c.Spec.AllowedNamespaces {
		if ok, err := path.Match(pattern, ns); err == nil && ok {
			return true
		}
	}
	return false
}
