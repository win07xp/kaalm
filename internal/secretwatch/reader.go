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

package secretwatch

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errListUnsupported is List's answer: the watcher serves named reads only.
var errListUnsupported = errors.New("secretwatch: List is not supported")

// Reader adapts a Watcher to client.Reader so code written against a
// controller-runtime reader can take it. Get serves *corev1.Secret only and
// passes the watcher's errors through unchanged: an absent Secret is an
// apierrors NotFound (from the synced cache or the live fallback) and a
// forbidden read is the live read's apierrors Forbidden.
type Reader struct {
	Watcher *Watcher
}

var _ client.Reader = Reader{}

// NewReader wraps w.
func NewReader(w *Watcher) Reader { return Reader{Watcher: w} }

// Get copies the Secret named by key into obj, which must be a
// *corev1.Secret. The copy leaves the watcher's cached object untouched.
func (r Reader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return fmt.Errorf("secretwatch: Get supports *corev1.Secret only, got %T", obj)
	}
	got, err := r.Watcher.Get(ctx, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	got.DeepCopyInto(sec)
	return nil
}

// List always fails: a list needs a list grant the callers do not hold.
func (Reader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errListUnsupported
}
