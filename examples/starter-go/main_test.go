// Copyright 2026. Licensed under the Apache License, Version 2.0.

package main

import "testing"

func TestDedupBuffer_CachesAndEvicts(t *testing.T) {
	d := newDedupBuffer(2)
	if _, ok := d.get("m1"); ok {
		t.Fatal("empty buffer must miss")
	}
	d.put("m1", ResponseEnvelope{Content: "one"})
	if r, ok := d.get("m1"); !ok || r.Content != "one" {
		t.Fatalf("cached reply wrong: %+v ok=%v", r, ok)
	}
	d.put("m2", ResponseEnvelope{Content: "two"})
	d.put("m3", ResponseEnvelope{Content: "three"}) // evicts the LRU (m1)
	if _, ok := d.get("m1"); ok {
		t.Error("m1 should have been evicted")
	}
	if _, ok := d.get("m3"); !ok {
		t.Error("m3 must be present")
	}
}

func TestShouldHeartbeat(t *testing.T) {
	t.Setenv("AGENTRY_TEMPLATE_HEARTBEAT", "auto")
	if !(&agent{isTask: false}).shouldHeartbeat() {
		t.Error("agent mode auto must heartbeat")
	}
	if (&agent{isTask: true}).shouldHeartbeat() {
		t.Error("task mode auto must not heartbeat")
	}
	t.Setenv("AGENTRY_TEMPLATE_HEARTBEAT", "off")
	if (&agent{isTask: false}).shouldHeartbeat() {
		t.Error("off must never heartbeat")
	}
}

func TestTaskAutocompleteStatus(t *testing.T) {
	if got := taskAutocompleteStatus(false, "success"); got != "" {
		t.Errorf("agent mode must never auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, ""); got != "" {
		t.Errorf("unset env must not auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, "success"); got != "success" {
		t.Errorf("task mode with env must return the status, got %q", got)
	}
}
