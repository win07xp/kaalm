/*
Copyright 2026.

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
	"net/http"
	"testing"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestUTF8ErrorOffset(t *testing.T) {
	if off := utf8ErrorOffset([]byte("hello")); off != -1 {
		t.Errorf("valid UTF-8 must return -1, got %d", off)
	}
	// 0xff is never a valid UTF-8 byte; here at offset 3.
	if off := utf8ErrorOffset([]byte{'a', 'b', 'c', 0xff, 'd'}); off != 3 {
		t.Errorf("bad byte offset = %d, want 3", off)
	}
}

func TestDottedLookup(t *testing.T) {
	body := map[string]any{
		"user":  map[string]any{"id": "u-1"},
		"count": float64(42),
		"ok":    true,
		"nested": map[string]any{
			"deep": map[string]any{"leaf": "found"},
		},
		"notmap": "scalar",
	}
	if got := dottedLookup(body, "user.id"); got != "u-1" {
		t.Errorf("string lookup = %q", got)
	}
	if got := dottedLookup(body, "count"); got != "42" {
		t.Errorf("float lookup = %q", got)
	}
	if got := dottedLookup(body, "ok"); got != "true" {
		t.Errorf("bool lookup = %q", got)
	}
	if got := dottedLookup(body, "nested.deep.leaf"); got != "found" {
		t.Errorf("deep lookup = %q", got)
	}
	// Missing intermediate.
	if got := dottedLookup(body, "user.absent"); got != "" {
		t.Errorf("missing leaf = %q", got)
	}
	// Intermediate is not a map.
	if got := dottedLookup(body, "notmap.x"); got != "" {
		t.Errorf("non-map intermediate = %q", got)
	}
	// Leaf is a map/other type: unsupported, empty string.
	if got := dottedLookup(body, "user"); got != "" {
		t.Errorf("map leaf must be empty = %q", got)
	}
}

func TestExtract(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-User", "hdr-user")
	fallback := "anon"

	// FromHeader.
	hdrName := "X-User"
	if got := extract(kaalmv1alpha1.ChannelExtractor{FromHeader: &hdrName}, r, nil); got != "hdr-user" {
		t.Errorf("header extract = %q", got)
	}
	// FromHeader missing -> fallback.
	missing := "X-Absent"
	if got := extract(kaalmv1alpha1.ChannelExtractor{FromHeader: &missing, Fallback: &fallback}, r, nil); got != "anon" {
		t.Errorf("fallback = %q", got)
	}
	// FromBody.
	path := "user.id"
	body := map[string]any{"user": map[string]any{"id": "b-user"}}
	if got := extract(kaalmv1alpha1.ChannelExtractor{FromBody: &path}, r, body); got != "b-user" {
		t.Errorf("body extract = %q", got)
	}
	// No extractor and no fallback -> empty.
	if got := extract(kaalmv1alpha1.ChannelExtractor{}, r, nil); got != "" {
		t.Errorf("empty extractor = %q", got)
	}
}
