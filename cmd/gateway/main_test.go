// Command gateway is the Agentry Gateway: the LLM listener on :8443 with
// per-path client authentication, the provider proxy, and a dedicated health
// port. The User listener (:8080) and the controller-facing internal handlers
// land in later phases. See docs/src/gateways/.
package main

import (
	"bytes"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestParseBackoff(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []time.Duration
	}{
		{
			name: "empty string returns nil (Config default)",
			raw:  "",
			want: nil,
		},
		{
			name: "single duration",
			raw:  "1s",
			want: []time.Duration{time.Second},
		},
		{
			name: "typical backoff schedule",
			raw:  "1s,5s,25s",
			want: []time.Duration{time.Second, 5 * time.Second, 25 * time.Second},
		},
		{
			name: "entries with surrounding whitespace are trimmed",
			raw:  " 1s , 5s ,25s ",
			want: []time.Duration{time.Second, 5 * time.Second, 25 * time.Second},
		},
		{
			name: "malformed entries are skipped, valid ones kept",
			raw:  "1s,not-a-duration,25s",
			want: []time.Duration{time.Second, 25 * time.Second},
		},
		{
			name: "all malformed entries yields an empty (non-nil) slice",
			raw:  "garbage,more-garbage",
			want: []time.Duration{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			got := parseBackoff(tt.raw, logger)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseBackoff(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseBackoffLogsMalformedEntries(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	got := parseBackoff("1s,bogus", logger)

	if want := []time.Duration{time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parseBackoff = %#v, want %#v", got, want)
	}
	if !bytes.Contains(buf.Bytes(), []byte("ignoring malformed backoff entry")) {
		t.Errorf("expected a warning to be logged for the malformed entry, got log output: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("bogus")) {
		t.Errorf("expected the log to mention the offending value %q, got: %s", "bogus", buf.String())
	}
}
