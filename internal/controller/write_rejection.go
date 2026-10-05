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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// isWriteRejection reports whether a write failed because the API server or
// an admission plugin refused it on purpose: Forbidden (a ResourceQuota, a
// missing RuntimeClass, PodSecurity, an admission webhook's or policy's
// denial, the controller's own RBAC), Invalid, or BadRequest. A backoff
// retry cannot clear these; only a change to the cluster or the spec can,
// so the caller reports them in status. Conflict (an update race),
// AlreadyExists (handled by createControlled, or cache lag on a
// GenerateName Pod), timeouts, 429, and 5xx (including a webhook that
// cannot be reached) are transient and stay reconcile errors.
func isWriteRejection(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}

// ChildWriteRejectedError reports that the API server rejected a create,
// update, or delete of a workload's child object other than the Pod. The
// reconcilers report it as Ready=False ChildWriteRejected; Unwrap keeps the
// API error reachable for apierrors checks.
type ChildWriteRejectedError struct {
	// Op is "creating", "updating", or "deleting".
	Op string
	// Kind and Name identify the child object.
	Kind, Name string
	// Err is the API server's error.
	Err error
}

func (e *ChildWriteRejectedError) Error() string {
	return fmt.Sprintf("%s %s %q: %v", e.Op, e.Kind, e.Name, e.Err)
}

func (e *ChildWriteRejectedError) Unwrap() error { return e.Err }

// asChildWriteRejected unwraps a ChildWriteRejectedError from err.
func asChildWriteRejected(err error) (*ChildWriteRejectedError, bool) {
	var cr *ChildWriteRejectedError
	if errors.As(err, &cr) {
		return cr, true
	}
	return nil, false
}

// rejectedWrite wraps err in a ChildWriteRejectedError naming op and obj
// when it is a rejection (isWriteRejection), and returns it unchanged
// otherwise, nil included.
func rejectedWrite(op string, scheme *runtime.Scheme, obj client.Object, err error) error {
	if !isWriteRejection(err) {
		return err
	}
	return &ChildWriteRejectedError{Op: op, Kind: kindOf(scheme, obj), Name: obj.GetName(), Err: err}
}
