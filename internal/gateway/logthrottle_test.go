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
	"testing"
	"time"
)

func TestLogThrottle_PacesPerKey(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var th logThrottle // zero value works without setup
	th.now = func() time.Time { return clock }

	if !th.allow("a", time.Minute) {
		t.Fatal("first allow for a key should be true")
	}
	clock = clock.Add(59 * time.Second)
	if th.allow("a", time.Minute) {
		t.Error("allow within the interval should be false")
	}
	if !th.allow("b", time.Minute) {
		t.Error("another key is paced separately and should be allowed")
	}
	clock = clock.Add(time.Second) // exactly one interval after the first line
	if !th.allow("a", time.Minute) {
		t.Error("allow exactly one interval later should be true")
	}
	if th.allow("a", time.Minute) {
		t.Error("allow right after a granted line should be false")
	}
}

func TestLogThrottle_ZeroValueUsesWallClock(t *testing.T) {
	var th logThrottle
	if !th.allow("a", time.Hour) {
		t.Fatal("first allow should be true")
	}
	if th.allow("a", time.Hour) {
		t.Error("second allow within an hour should be false")
	}
}
