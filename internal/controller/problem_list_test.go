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
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// conditionMessageLimit is the condition schema's maxLength for
// status.conditions[].message. A longer message fails the whole status write.
// It is a literal, not the production constant, so the tests check the
// schema value on their own.
const conditionMessageLimit = 32768

func TestEntryList(t *testing.T) {
	const one, many = "1 more", "%d more"
	fill := func(keys ...string) *entryList {
		var l entryList
		for _, k := range keys {
			l.add(k, "msg "+k)
		}
		return &l
	}
	keys := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "k" + strconv.Itoa(i)
		}
		return out
	}
	msgs := func(ks []string) []string {
		out := make([]string, len(ks))
		for i, k := range ks {
			out[i] = "msg " + k
		}
		return out
	}
	cases := []struct {
		name string
		keys []string
		want []string
	}{
		{"empty", nil, nil},
		{"add order kept", []string{"b", "a", "c"}, []string{"msg b", "msg a", "msg c"}},
		{"repeat neither listed nor counted", []string{"a", "a", "b", "a"}, []string{"msg a", "msg b"}},
		{"ten listed, no count", keys(10), msgs(keys(10))},
		{"eleventh counted once", keys(11), append(msgs(keys(10)), "1 more")},
		{"rest counted", keys(14), append(msgs(keys(10)), "4 more")},
		{"repeat of a counted key not counted again", append(keys(12), "k11", "k10"), append(msgs(keys(10)), "2 more")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fill(tc.keys...).messages(one, many); !slices.Equal(got, tc.want) {
				t.Errorf("messages = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinProblems(t *testing.T) {
	if got := joinProblems([]string{"b", "c", "a"}); got != "a; b; c" {
		t.Errorf("joinProblems = %q, want sorted and joined", got)
	}
	exact := strings.Repeat("a", conditionMessageLimit)
	if got := joinProblems([]string{exact}); got != exact {
		t.Errorf("a message of exactly %d bytes was changed to %d bytes", conditionMessageLimit, len(got))
	}
	// The cut lands inside a three-byte rune.
	long := strings.Repeat("€", conditionMessageLimit)
	got := joinProblems([]string{long})
	if len(got) > conditionMessageLimit {
		t.Errorf("cut message is %d bytes, want at most %d", len(got), conditionMessageLimit)
	}
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("cut message does not end in the cut marker")
	}
	if !utf8.ValidString(got) {
		t.Errorf("cut message is not valid UTF-8")
	}
}
