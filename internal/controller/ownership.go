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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// ChildConflictError reports that a workload's child name is taken by an
// object the workload does not control. The reconcilers find children by a
// predictable name, so without this check a workload name could steer the
// controller onto an object someone else owns (a platform NetworkPolicy, for
// one). The pass must neither update nor delete the object, and the workload
// must not start its Pod.
type ChildConflictError struct {
	// Kind and Name identify the child object.
	Kind, Name string
	// OwnerKind is the workload kind, Agent or AgentTask.
	OwnerKind string
}

func (e *ChildConflictError) Error() string {
	return fmt.Sprintf("%s %q already exists and is not owned by this %s", e.Kind, e.Name, e.OwnerKind)
}

// asChildConflict unwraps a ChildConflictError from err.
func asChildConflict(err error) (*ChildConflictError, bool) {
	var cc *ChildConflictError
	if errors.As(err, &cc) {
		return cc, true
	}
	return nil, false
}

// requireControlled returns a ChildConflictError when owner is not the
// controller of obj. Call it on every existing child before an update, a
// delete, or treating the child as already converged.
func requireControlled(scheme *runtime.Scheme, owner, obj client.Object) error {
	if metav1.IsControlledBy(obj, owner) {
		return nil
	}
	return &ChildConflictError{Kind: kindOf(scheme, obj), Name: obj.GetName(), OwnerKind: kindOf(scheme, owner)}
}

// createControlled creates obj, which already carries the owner's controller
// reference. When the name is taken it reads the existing object and returns
// a ChildConflictError unless owner controls it. A create the API server
// rejects (isWriteRejection) comes back as a ChildWriteRejectedError.
func createControlled(ctx context.Context, c client.Client, owner, obj client.Object) error {
	err := c.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return verifyControlled(ctx, c, owner, obj)
	}
	return rejectedWrite("creating", c.Scheme(), obj, err)
}

// verifyControlled reads the object named like obj and checks that owner
// controls it. obj itself is left unchanged.
func verifyControlled(ctx context.Context, c client.Client, owner, obj client.Object) error {
	current, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", obj)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, current); err != nil {
		return err
	}
	return requireControlled(c.Scheme(), owner, current)
}

// kindOf names an object's kind for a message, falling back to its Go type.
func kindOf(scheme *runtime.Scheme, obj runtime.Object) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	if scheme != nil {
		if gvk, err := apiutil.GVKForObject(obj, scheme); err == nil {
			return gvk.Kind
		}
	}
	return fmt.Sprintf("%T", obj)
}
