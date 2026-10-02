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

import "testing"

func TestAgentClass_AdmitsNamespace(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		ns       string
		want     bool
	}{
		{"unset admits any namespace", nil, "anything", true},
		{"exact match admits", []string{"team-a"}, "team-a", true},
		{"exact miss refuses", []string{"team-a"}, "team-b", false},
		{"glob admits", []string{"team-*"}, "team-x", true},
		{"glob miss refuses", []string{"team-*"}, "prod", false},
		{"star admits all", []string{"*"}, "prod", true},
		{"second pattern admits", []string{"team-*", "default"}, "default", true},
		{"malformed pattern admits nothing", []string{"["}, "team-a", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class := &AgentClass{Spec: AgentClassSpec{AllowedNamespaces: c.patterns}}
			if got := class.AdmitsNamespace(c.ns); got != c.want {
				t.Errorf("AdmitsNamespace(%q) with %v = %v, want %v", c.ns, c.patterns, got, c.want)
			}
		})
	}
}
