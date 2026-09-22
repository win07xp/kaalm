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
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ciliumPolicyGVK is the CiliumNetworkPolicy kind. The object is handled as
// unstructured so the Cilium types are not vendored.
var ciliumPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

// fqdnPolicyName is the name of a workload's FQDN egress policy.
func fqdnPolicyName(owner string) string { return owner + "-fqdn" }

// dnsEndpointLabels selects the cluster DNS Pods in Cilium's label syntax.
// Cilium learns the IPs behind a toFQDNs name only from DNS traffic its proxy
// sees, so the policy must allow DNS to these endpoints with a dns rule. The
// values are the defaults the NetworkPolicy DNS peer uses; when the DNS
// selector becomes configurable (#264), wire it in here.
func dnsEndpointLabels() map[string]any {
	return map[string]any{
		"k8s:io.kubernetes.pod.namespace": metav1.NamespaceSystem,
		"k8s:k8s-app":                     "kube-dns",
	}
}

// desiredFQDNPolicy builds the CiliumNetworkPolicy that lets a workload Pod
// reach the class's allowedHosts: DNS to the cluster resolver through
// Cilium's DNS proxy, and egress to each host on any port. It adds to the
// workload's NetworkPolicy; Cilium allows the union of both.
func desiredFQDNPolicy(owner client.Object, podLabels map[string]string, hosts []string) *unstructured.Unstructured {
	sorted := slices.Clone(hosts)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	fqdns := make([]any, 0, len(sorted))
	for _, h := range sorted {
		fqdns = append(fqdns, map[string]any{"matchName": h})
	}
	selector := make(map[string]any, len(podLabels))
	for k, v := range podLabels {
		selector[k] = v
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumPolicyGVK)
	u.SetName(fqdnPolicyName(owner.GetName()))
	u.SetNamespace(owner.GetNamespace())
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": selector},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": dnsEndpointLabels()}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
					"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
				}},
			},
			map[string]any{"toFQDNs": fqdns},
		},
	}
	return u
}

// ensureFQDNPolicy converges the owner's CiliumNetworkPolicy. With support and
// hosts it creates or updates the policy; with support and no hosts it deletes
// any policy left from an earlier spec. Without support it does nothing: the
// kind may not exist, so even a read would fail. A cluster whose FQDN-capable
// CNI is not Cilium has no CiliumNetworkPolicy kind either, so a no-match
// error is also nothing to do.
func ensureFQDNPolicy(
	ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner client.Object, podLabels map[string]string, hosts []string, supported bool,
) error {
	if !supported {
		return nil
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(ciliumPolicyGVK)
	key := types.NamespacedName{Namespace: owner.GetNamespace(), Name: fqdnPolicyName(owner.GetName())}
	err := c.Get(ctx, key, current)
	if apimeta.IsNoMatchError(err) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	found := err == nil

	if len(hosts) == 0 {
		if !found {
			return nil
		}
		return client.IgnoreNotFound(c.Delete(ctx, current))
	}

	desired := desiredFQDNPolicy(owner, podLabels, hosts)
	if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
		return err
	}
	if !found {
		return c.Create(ctx, desired)
	}
	if equality.Semantic.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	current.Object["spec"] = desired.Object["spec"]
	return c.Update(ctx, current)
}
