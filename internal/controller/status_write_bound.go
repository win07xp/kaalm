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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// statusSubresource is the subresource whose writes carry status.conditions.
const statusSubresource = "status"

// NewManagerClient builds the manager's client: the default client, with
// every status write passed through cutConditionMessages first. It honors
// options.Cache the way client.New does, so reads still come from the
// manager's cache.
func NewManagerClient(config *rest.Config, options client.Options) (client.Client, error) {
	c, err := client.NewWithWatch(config, options)
	if err != nil {
		return nil, err
	}
	return boundStatusWrites(c), nil
}

// boundStatusWrites wraps c so that every status write (Status().Update and
// Status().Patch) of a Kaalm kind with status conditions cuts any condition
// message past the schema's limit before the request is sent. Without it, one
// such message fails the whole write and leaves Ready, the phase, and
// observedGeneration at their last values.
//
// The cut is made in the caller's object, in place. A patch built from the
// object, such as client.MergeFrom, is computed inside the wrapped call and
// so sees the cut; a raw or apply patch body is sent as built. Other
// subresources, other kinds, and unstructured objects pass through
// unchanged. Main-resource writes are not inspected: every Kaalm CRD enables
// the status subresource, so the API server ignores status on them.
//
// The per-site bounds (quotedEntry, entryList, cutDetail) stay the main
// bound and keep each message readable; this is the backstop, so no message
// ever fails a status write.
func boundStatusWrites(c client.WithWatch) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if subResource == statusSubresource {
				cutConditionMessages(ctx, obj)
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if subResource == statusSubresource {
				cutConditionMessages(ctx, obj)
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
}

// cutConditionMessages cuts, in obj itself, every status condition message
// longer than maxConditionMessageBytes. A message within the limit costs one
// length check and no allocation. Each cut is logged, since it marks a
// message whose own site let it grow past the limit.
func cutConditionMessages(ctx context.Context, obj client.Object) {
	conds := conditionsOf(obj)
	for i := range conds {
		if len(conds[i].Message) <= maxConditionMessageBytes {
			continue
		}
		log.FromContext(ctx).Info("cut a condition message to the condition schema's limit",
			"namespace", obj.GetNamespace(), "name", obj.GetName(),
			"condition", conds[i].Type, "bytes", len(conds[i].Message))
		conds[i].Message = cutConditionMessage(conds[i].Message)
	}
}

// conditionsOf returns the status conditions of a Kaalm kind that has them,
// sharing obj's backing array, or nil for any other object.
func conditionsOf(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *kaalmv1beta1.Agent:
		return o.Status.Conditions
	case *kaalmv1beta1.AgentTask:
		return o.Status.Conditions
	case *kaalmv1beta1.AgentChannel:
		return o.Status.Conditions
	case *kaalmv1beta1.AgentClass:
		return o.Status.Conditions
	case *kaalmv1beta1.ModelProvider:
		return o.Status.Conditions
	case *kaalmv1beta1.ToolProvider:
		return o.Status.Conditions
	}
	return nil
}
