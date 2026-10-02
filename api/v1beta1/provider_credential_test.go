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

// Rule 49: only the exact value "true" of kaalm.io/provider-credential opts a
// Secret in; other Kaalm labels do not count.
func TestProviderCredentialOptedIn(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{"kaalm.io/provider-credential": "true"}, true},
		{map[string]string{"kaalm.io/provider-credential": "True"}, false},
		{map[string]string{"kaalm.io/provider-credential": "yes"}, false},
		{map[string]string{"kaalm.io/provider-credential": ""}, false},
		{map[string]string{"kaalm.io/provider-credentials": "true"}, false},
		{map[string]string{"kaalm.io/channel-credential": "true"}, false},
	}
	for _, c := range cases {
		if got := ProviderCredentialOptedIn(c.labels); got != c.want {
			t.Errorf("ProviderCredentialOptedIn(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

// Rule 50: the endpoint host must be listed exactly in
// kaalm.io/provider-hosts, compared without case; list entries are trimmed,
// and there are no wildcards. kaalm.io/callback-hosts does not count.
func TestProviderHostApproved(t *testing.T) {
	const key = "kaalm.io/provider-hosts"
	cases := []struct {
		name        string
		annotations map[string]string
		host        string
		want        bool
	}{
		{"no annotations", nil, "api.example.com", false},
		{"empty list", map[string]string{key: ""}, "api.example.com", false},
		{"listed", map[string]string{key: "api.example.com"}, "api.example.com", true},
		{"one of several, spaces trimmed", map[string]string{key: "a.example.com, api.example.com ,b.example.com"}, "api.example.com", true},
		{"case-insensitive", map[string]string{key: "API.Example.COM"}, "api.example.com", true},
		{"not listed", map[string]string{key: "a.example.com"}, "api.example.com", false},
		{"no suffix match", map[string]string{key: "example.com"}, "api.example.com", false},
		{"no wildcard", map[string]string{key: "*.example.com"}, "api.example.com", false},
		{"no wildcard entry matches", map[string]string{key: "*"}, "api.example.com", false},
		{"trailing dot is another host", map[string]string{key: "api.example.com."}, "api.example.com", false},
		{"empty host", map[string]string{key: "api.example.com"}, "", false},
		{"empty entries ignored", map[string]string{key: ",,"}, "", false},
		{"callback-hosts annotation does not count", map[string]string{"kaalm.io/callback-hosts": "api.example.com"}, "api.example.com", false},
	}
	for _, c := range cases {
		if got := ProviderHostApproved(c.annotations, c.host); got != c.want {
			t.Errorf("%s: ProviderHostApproved(%v, %q) = %v, want %v", c.name, c.annotations, c.host, got, c.want)
		}
	}
}

// EndpointHost returns the hostname rule 50 checks: the host the gateway and
// the probes dial, without port or brackets, and "" when there is none.
func TestEndpointHost(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"https://api.example.com", "api.example.com"},
		{"https://mock-provider.e2e.svc:8443/ok", "mock-provider.e2e.svc"},
		{"https://[fd00::1]:8443", "fd00::1"},
		{"https://10.0.0.5", "10.0.0.5"},
		{"https://x@api.example.com", "api.example.com"},
		{"https://", ""},
		{"https://:8443", ""},
		{"http://api.example.com", ""},
		{"https://api.example.com:bad port", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := EndpointHost(c.endpoint); got != c.want {
			t.Errorf("EndpointHost(%q) = %q, want %q", c.endpoint, got, c.want)
		}
	}
}

// The gateway runs the rule 50 match on every provider request, so it must
// not allocate.
func TestProviderHostApprovedDoesNotAllocate(t *testing.T) {
	annotations := map[string]string{"kaalm.io/provider-hosts": "a.example.com, b.example.com ,API.example.com"}
	allocs := testing.AllocsPerRun(100, func() {
		if !ProviderHostApproved(annotations, "api.example.com") {
			t.Fatal("host not approved")
		}
	})
	if allocs != 0 {
		t.Fatalf("ProviderHostApproved allocates %v times per call, want 0", allocs)
	}
}
