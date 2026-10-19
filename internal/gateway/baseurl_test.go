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

package gateway

import (
	"strings"
	"testing"
)

func TestParseBaseURL(t *testing.T) {
	u, err := ParseBaseURL("some url", "https://h.example:8443/api/v10")
	if err != nil {
		t.Fatalf("ParseBaseURL = %v, want nil", err)
	}
	if u == nil || u.Host != "h.example:8443" || u.Path != "/api/v10" {
		t.Fatalf("ParseBaseURL = %+v, want host h.example:8443 and path /api/v10", u)
	}

	for _, tc := range []struct{ name, value, want string }{
		{"parse failure", "https://h .example/api", "some url: "},
		{"scheme", "ftp://h.example", `some url "ftp://h.example": need http(s)://host[:port][/path]`},
		{"query", "https://h.example/v1?", `some url "https://h.example/v1?": need http(s)://host[:port][/path] with no user info, query, or fragment`},
		{"port", "https://h.example:70000/v1", `some url "https://h.example:70000/v1": port 70000 is outside 1-65535`},
	} {
		_, err := ParseBaseURL("some url", tc.value)
		if err == nil {
			t.Errorf("%s: ParseBaseURL(%q) must error", tc.name, tc.value)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: ParseBaseURL(%q) = %q, want prefix %q", tc.name, tc.value, err, tc.want)
		}
	}
}
