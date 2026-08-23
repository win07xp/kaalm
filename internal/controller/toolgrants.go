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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// toolGrantViolations evaluates rules 35 to 38 for one workload's tool grants
// against its class, mirroring the provider checks of rules 3 to 5. Shared by
// the Agent reconciler (recoverable Degraded, all reasons collected) and the
// AgentTask reconciler (terminal Failed, first reason wins). See
// docs/src/gateways/tool-plane.md (Grants).
func toolGrantViolations(
	ctx context.Context, c client.Reader, namespace string,
	grants []kaalmv1beta1.AgentToolGrant, class *kaalmv1beta1.AgentClass,
) []metav1.Condition {
	var out []metav1.Condition
	add := func(reason, msg string) {
		out = append(out, metav1.Condition{Reason: reason, Message: msg})
	}
	for _, g := range grants {
		name := g.ProviderRef.Name
		// Rule 37: the class must allow the tool provider. An empty
		// allowedToolProviders list allows none.
		allowed := false
		for _, ap := range class.Spec.AllowedToolProviders {
			if ap.Name == name {
				allowed = true
				break
			}
		}
		if !allowed {
			add(kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("tool provider %q is not in AgentClass %q allowedToolProviders", name, class.Name))
			continue
		}
		// Rule 35: the reference must resolve.
		var tp kaalmv1beta1.ToolProvider
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &tp); err != nil {
			if apierrors.IsNotFound(err) {
				add(kaalmv1beta1.ReasonClassConstraintViolation,
					fmt.Sprintf("tool provider %q does not exist", name))
			}
			continue
		}
		// Rule 36: the provider must admit the workload's namespace.
		if !namespaceAllowed(namespace, tp.Spec.AllowedNamespaces) {
			add(kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("tool provider %q does not allow namespace %q", name, namespace))
			continue
		}
		// Rule 38: when the provider declares a catalog, every granted tool
		// name must appear in it. No catalog means the server's own
		// tools/list governs and the rule does not apply.
		if len(tp.Spec.Tools) > 0 && len(g.Tools) > 0 {
			catalog := make(map[string]bool, len(tp.Spec.Tools))
			for _, t := range tp.Spec.Tools {
				catalog[t.ID] = true
			}
			var missing []string
			for _, t := range g.Tools {
				if !catalog[t] {
					missing = append(missing, t)
				}
			}
			if len(missing) > 0 {
				add(kaalmv1beta1.ReasonToolNotInCatalog,
					fmt.Sprintf("granted tools %v are not in the declared catalog of tool provider %q", missing, name))
			}
		}
	}
	return out
}
