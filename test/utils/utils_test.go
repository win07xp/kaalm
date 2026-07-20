package utils

import "testing"

func TestParseForwardPort(t *testing.T) {
	p, ok := parseForwardPort("Forwarding from 127.0.0.1:34567 -> 8080")
	if !ok || p != 34567 {
		t.Fatalf("got (%d,%v), want (34567,true)", p, ok)
	}
	if _, ok := parseForwardPort("Forwarding from [::1]:34567 -> 8080"); ok {
		t.Error("must only match the IPv4 line to avoid a duplicate port")
	}
	if _, ok := parseForwardPort("random log line"); ok {
		t.Error("non-matching line must return false")
	}
}
