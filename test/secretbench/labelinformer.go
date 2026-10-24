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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// channelCredentialSelector selects the Secrets that opted in to AgentChannel
// use.
const channelCredentialSelector = kaalmv1beta1.LabelChannelCredential + "=true"

// labelInformerReader is prototype A: one informer per namespace that LISTs
// and WATCHes the Secrets labeled kaalm.io/channel-credential=true and serves
// every read from its store. The watch count follows the namespaces read,
// not the Secrets. It serves only labeled Secrets: an unlabeled Secret is
// NotFound, because the store never holds it. Shipping it would need list
// and watch on labeled Secrets in each namespace, the RBAC change the
// security pages would have to describe; the benchmark measures cost, not
// RBAC.
type labelInformerReader struct {
	client kubernetes.Interface

	mu      sync.Mutex
	entries map[string]*informerEntry
}

func newLabelInformerReader(_ context.Context, client kubernetes.Interface) *labelInformerReader {
	return &labelInformerReader{client: client, entries: map[string]*informerEntry{}}
}

func (r *labelInformerReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return r.entryFor(namespace).read(ctx, r.client, namespace, name)
}

func (r *labelInformerReader) entryFor(namespace string) *informerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[namespace]; ok {
		return e
	}
	e := &informerEntry{stop: make(chan struct{})}
	secrets := r.client.CoreV1().Secrets(namespace)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.LabelSelector = channelCredentialSelector
			list, err := secrets.List(ctx, opts)
			e.recordList(err)
			if err != nil {
				return nil, err
			}
			return list, nil
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.LabelSelector = channelCredentialSelector
			return secrets.Watch(ctx, opts)
		},
	}
	e.informer = cache.NewSharedIndexInformer(listThenWatch{lw}, &corev1.Secret{}, 0, cache.Indexers{})
	go e.informer.Run(e.stop)
	r.entries[namespace] = e
	return e
}

func (r *labelInformerReader) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ns, e := range r.entries {
		close(e.stop)
		delete(r.entries, ns)
	}
}
