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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const phaseSelector = labelPhase + "=" + phaseValue

func secretName(i int) string { return fmt.Sprintf("secret-%05d", i) }

// namespaceFor is the namespace of Secret i: the one namespace, or a
// round-robin over the spread namespaces.
func namespaceFor(i int, layout string, namespaces int) string {
	if layout == layoutOne {
		return oneNamespace
	}
	return fmt.Sprintf(spreadNamespaceFormat, i%namespaces)
}

// namespacesUsed is how many namespaces count Secrets occupy.
func namespacesUsed(count int, layout string, namespaces int) int {
	if count == 0 {
		return 0
	}
	if layout == layoutOne {
		return 1
	}
	return min(count, namespaces)
}

// layoutNamespaces lists every namespace a layout can use.
func layoutNamespaces(layout string, namespaces int) []string {
	if layout == layoutOne {
		return []string{oneNamespace}
	}
	out := make([]string, namespaces)
	for i := range out {
		out[i] = fmt.Sprintf(spreadNamespaceFormat, i)
	}
	return out
}

// secretKeys lists the first count Secrets of a layout.
func secretKeys(count int, layout string, namespaces int) []types.NamespacedName {
	out := make([]types.NamespacedName, count)
	for i := range out {
		out[i] = types.NamespacedName{Namespace: namespaceFor(i, layout, namespaces), Name: secretName(i)}
	}
	return out
}

// ensureNamespaces creates the namespaces once. They are never deleted:
// envtest runs no namespace controller, so a deleted namespace would stay
// Terminating with its Secrets inside.
func ensureNamespaces(ctx context.Context, admin kubernetes.Interface, layouts []string, namespaces int) error {
	for _, layout := range layouts {
		for _, ns := range layoutNamespaces(layout, namespaces) {
			obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: ns, Labels: map[string]string{labelPhase: phaseValue},
			}}
			_, err := admin.CoreV1().Namespaces().Create(ctx, obj, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create namespace %s: %w", ns, err)
			}
		}
	}
	return nil
}

func newToken() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return []byte(hex.EncodeToString(b)) // 32 bytes
}

// benchSecret is a Secret labeled as a channel credential, as a channel's
// bearer or HMAC Secret is, and as the benchmark's own.
func benchSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				kaalmv1beta1.LabelChannelCredential: "true",
				labelPhase:                          phaseValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": newToken()},
	}
}

// ensureSecrets creates Secrets from..to-1 of a layout with 16 workers, each
// labeled as a channel credential, as a channel's bearer or HMAC Secret is.
func ensureSecrets(ctx context.Context, admin kubernetes.Interface, layout string, namespaces, from, to int) error {
	if from >= to {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for i := range jobs {
				sec := benchSecret(namespaceFor(i, layout, namespaces), secretName(i))
				_, err := admin.CoreV1().Secrets(sec.Namespace).Create(ctx, sec, metav1.CreateOptions{})
				if err != nil && !apierrors.IsAlreadyExists(err) {
					select {
					case errs <- fmt.Errorf("create Secret %s/%s: %w", sec.Namespace, sec.Name, err):
					default:
					}
					cancel()
				}
			}
		})
	}
feed:
	for i := from; i < to; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return ctx.Err()
	}
}

// clearSecrets deletes the benchmark's Secrets from namespaces and waits
// until none is left.
func clearSecrets(ctx context.Context, admin kubernetes.Interface, namespaces []string) error {
	for _, ns := range namespaces {
		err := admin.CoreV1().Secrets(ns).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: phaseSelector})
		if err != nil {
			return fmt.Errorf("clear Secrets in %s: %w", ns, err)
		}
	}
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			for _, ns := range namespaces {
				list, err := admin.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: phaseSelector, Limit: 1})
				if err != nil {
					return false, err
				}
				if len(list.Items) > 0 {
					return false, nil
				}
			}
			return true, nil
		})
}

// updateSecret writes a new token to a Secret and returns its new
// resourceVersion.
func updateSecret(ctx context.Context, admin kubernetes.Interface, namespace, name string) (string, error) {
	sec, err := admin.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sec.Data = map[string][]byte{"token": newToken()}
	updated, err := admin.CoreV1().Secrets(namespace).Update(ctx, sec, metav1.UpdateOptions{})
	if err != nil {
		return "", err
	}
	return updated.ResourceVersion, nil
}
