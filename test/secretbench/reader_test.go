//go:build secretbench

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

package main

import (
	"context"
	"testing"
)

// The Watcher lists the Secret by name and watches from that list, with no
// GET. This pins the request mix the benchmark reports and checks the
// counting transport against the product code.
func TestWatcherReaderRequestsOneListAndOneWatch(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	client, counter := countedClient(t)
	r := newWatcherReader(ctx, client)

	createSecret(t, ns, "one")
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	settle(t, counter, 1)
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests = %+v, want List 1, Watch 1", got)
	}
	stopAndDrain(t, r, counter)
}
