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

import apierrors "k8s.io/apimachinery/pkg/api/errors"

// isPodCreateRejection reports whether a Pod create failed because the API
// server or an admission plugin refused the object on purpose: Forbidden
// (a missing RuntimeClass, a ResourceQuota, PodSecurity, an admission
// webhook's denial, the controller's own RBAC), Invalid, or BadRequest. A
// backoff retry cannot clear these; only a change to the cluster or the spec
// can, so the caller reports them in status. Conflicts, AlreadyExists (cache
// lag on a GenerateName Pod), timeouts, 429, and 5xx (including a webhook
// that cannot be reached) are transient and stay reconcile errors.
func isPodCreateRejection(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}
