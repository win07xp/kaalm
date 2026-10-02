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

// Rule 45: only the exact value "true" opts a Secret in.
func TestChannelCredentialOptedIn(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{"kaalm.io/channel-credential": "true"}, true},
		{map[string]string{"kaalm.io/channel-credential": "True"}, false},
		{map[string]string{"kaalm.io/channel-credential": "yes"}, false},
		{map[string]string{"kaalm.io/channel-credential": ""}, false},
		{map[string]string{"kaalm.io/channel-credentials": "true"}, false},
	}
	for _, c := range cases {
		if got := ChannelCredentialOptedIn(c.labels); got != c.want {
			t.Errorf("ChannelCredentialOptedIn(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

// Rule 46: the callback host must be listed exactly, compared without case;
// list entries are trimmed, and there are no wildcards.
func TestCallbackHostApproved(t *testing.T) {
	const key = "kaalm.io/callback-hosts"
	cases := []struct {
		name        string
		annotations map[string]string
		host        string
		want        bool
	}{
		{"no annotations", nil, "hooks.example.com", false},
		{"empty list", map[string]string{key: ""}, "hooks.example.com", false},
		{"listed", map[string]string{key: "hooks.example.com"}, "hooks.example.com", true},
		{"one of several, spaces trimmed", map[string]string{key: "a.example.com, hooks.example.com ,b.example.com"}, "hooks.example.com", true},
		{"case-insensitive", map[string]string{key: "Hooks.Example.COM"}, "hooks.example.com", true},
		{"not listed", map[string]string{key: "a.example.com"}, "hooks.example.com", false},
		{"no suffix match", map[string]string{key: "example.com"}, "hooks.example.com", false},
		{"no wildcard", map[string]string{key: "*.example.com"}, "hooks.example.com", false},
		{"no wildcard entry matches a literal star host", map[string]string{key: "*"}, "hooks.example.com", false},
		{"empty host", map[string]string{key: "hooks.example.com"}, "", false},
		{"empty entries ignored", map[string]string{key: ",,"}, "", false},
	}
	for _, c := range cases {
		if got := CallbackHostApproved(c.annotations, c.host); got != c.want {
			t.Errorf("%s: CallbackHostApproved(%v, %q) = %v, want %v", c.name, c.annotations, c.host, got, c.want)
		}
	}
}
