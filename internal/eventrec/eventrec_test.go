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

package eventrec

import (
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits", "short", 10, "short"},
		{"exact", "0123456789", 10, "0123456789"},
		{"cut", strings.Repeat("a", 40), 30, strings.Repeat("a", 30-len(truncatedSuffix)) + truncatedSuffix},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.max); got != c.want {
			t.Errorf("%s: Truncate = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTruncate_KeepsUTF8Whole(t *testing.T) {
	in := strings.Repeat("é", 100) // two bytes each
	got := Truncate(in, 51)
	if len(got) > 51 {
		t.Fatalf("len = %d, want at most 51", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cut through a rune: %q", got)
	}
}

func TestBounded_TruncatesTheFormattedNote(t *testing.T) {
	fake := events.NewFakeRecorder(1)
	Bounded(fake).Eventf(&corev1.Pod{}, nil, corev1.EventTypeWarning, "R", "A", "%s", strings.Repeat("x", 3000))
	got := <-fake.Events
	note := strings.TrimPrefix(got, "Warning R ")
	if len(note) > MaxNoteBytes || !strings.HasSuffix(note, truncatedSuffix) {
		t.Fatalf("note is %d bytes, want at most %d and the truncation marker", len(note), MaxNoteBytes)
	}
}

func TestBounded_PassesANoteWithoutArgsVerbatim(t *testing.T) {
	fake := events.NewFakeRecorder(1)
	Bounded(fake).Eventf(&corev1.Pod{}, nil, corev1.EventTypeNormal, "R", "A", "100% of https://x/a%20b")
	if got := <-fake.Events; got != "Normal R 100% of https://x/a%20b" {
		t.Fatalf("got %q", got)
	}
}
