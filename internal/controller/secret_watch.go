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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// SecretChangeSource reports changes to the Secrets it watches. A
// secretwatch.Watcher satisfies it.
type SecretChangeSource interface {
	Subscribe(fn func(types.NamespacedName))
}

// secretChangeSource is the controller source for a referenced-Secret
// watch: each change that changes reports enqueues the requests that
// referrers returns for the Secret. The controller cannot list
// user-namespace Secrets, so the watch rides on the per-Secret informers
// the reads already started. The subscriber runs on the informer's handler
// goroutine, so referrers must do only indexed cache lists.
func secretChangeSource(
	changes SecretChangeSource,
	referrers func(ctx context.Context, key types.NamespacedName) []reconcile.Request,
) source.Source {
	return source.Func(func(
		ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request],
	) error {
		changes.Subscribe(func(key types.NamespacedName) {
			if ctx.Err() != nil {
				return
			}
			for _, req := range referrers(ctx, key) {
				q.Add(req)
			}
		})
		return nil
	})
}

// classesNamingPullSecret returns the names of the AgentClasses whose
// spec.image.imagePullSecrets name the Secret, through the
// IndexClassPullSecret field index.
func classesNamingPullSecret(ctx context.Context, c client.Reader, name string) ([]string, error) {
	var classes kaalmv1beta1.AgentClassList
	if err := c.List(ctx, &classes, client.MatchingFields{IndexClassPullSecret: name}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(classes.Items))
	for i := range classes.Items {
		names = append(names, classes.Items[i].Name)
	}
	return names, nil
}

// workloadsForSecret maps a changed Secret to the Agents or AgentTasks in
// its namespace that read it: those whose env names it (IndexEnvSecretRef)
// and those whose class lists it as a pull Secret (IndexClassPullSecret,
// then IndexAgentClassRef). list lists the workload kind with the given
// options and returns the keys of the items. Each workload appears once. A
// list error is logged and ends the lookup with what was found so far; the
// periodic pass still re-checks the workloads.
func workloadsForSecret(
	ctx context.Context, c client.Reader, key types.NamespacedName,
	list func(ctx context.Context, opts ...client.ListOption) ([]types.NamespacedName, error),
) []reconcile.Request {
	var reqs []reconcile.Request
	seen := map[types.NamespacedName]bool{}
	add := func(keys []types.NamespacedName) {
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				reqs = append(reqs, reconcile.Request{NamespacedName: k})
			}
		}
	}
	logErr := func(err error) []reconcile.Request {
		log.FromContext(ctx).Error(err, "listing workloads that read a Secret", "secret", key)
		return reqs
	}
	keys, err := list(ctx, client.InNamespace(key.Namespace), client.MatchingFields{IndexEnvSecretRef: key.Name})
	if err != nil {
		return logErr(err)
	}
	add(keys)
	classes, err := classesNamingPullSecret(ctx, c, key.Name)
	if err != nil {
		return logErr(err)
	}
	for _, class := range classes {
		keys, err := list(ctx, client.InNamespace(key.Namespace), client.MatchingFields{IndexAgentClassRef: class})
		if err != nil {
			return logErr(err)
		}
		add(keys)
	}
	return reqs
}
