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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/win07xp/kaalm/internal/secretwatch"
)

// The client limit the benchmark runs the Watcher at by default: the
// gateway's --client-qps and --client-burst defaults (cmd/gateway/main.go),
// which the controller's Secret watcher also runs at on a client of its own
// (secretWatcherQPS and secretWatcherBurst in cmd/manager/main.go).
const (
	gatewayQPS   = 100
	gatewayBurst = 200
)

// watcherReader is the shipped secretwatch.Watcher with a way to stop it.
// Stop cancels the watcher's context, on which its janitor stops every
// informer; the benchmark then waits for the goroutines and watches to
// drain before the next step.
type watcherReader struct {
	w      *secretwatch.Watcher
	cancel context.CancelFunc
}

// newWatcherReader starts a cold Watcher on client with its defaults.
func newWatcherReader(ctx context.Context, client kubernetes.Interface) *watcherReader {
	ctx, cancel := context.WithCancel(ctx)
	return &watcherReader{w: secretwatch.New(ctx, client), cancel: cancel}
}

func (r *watcherReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return r.w.Get(ctx, namespace, name)
}

func (r *watcherReader) Stop() { r.cancel() }
