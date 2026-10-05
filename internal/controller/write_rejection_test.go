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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Only a deliberate refusal of a write is a rejection; anything a backoff
// retry can clear stays an error.
func TestIsWriteRejection(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"forbidden", apierrors.NewForbidden(pods, "p", errors.New(`pod rejected: RuntimeClass "x" not found`)), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "p", field.ErrorList{field.Required(field.NewPath("spec"), "")}), true},
		{"bad request", apierrors.NewBadRequest("bad pod"), true},
		{"conflict", apierrors.NewConflict(pods, "p", errors.New("conflict")), false},
		{"already exists", apierrors.NewAlreadyExists(pods, "p"), false},
		{"server timeout", apierrors.NewServerTimeout(pods, "create", 1), false},
		{"timeout", apierrors.NewTimeoutError("timeout", 1), false},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"webhook unreachable", apierrors.NewInternalError(errors.New(`failed calling webhook "w": connection refused`)), false},
		{"service unavailable", apierrors.NewServiceUnavailable("unavailable"), false},
		{"plain error", fmt.Errorf("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		if got := isWriteRejection(tc.err); got != tc.want {
			t.Errorf("%s: isWriteRejection(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// rejectedWrite wraps a rejection with the operation and the child it hit,
// keeps the API error reachable, and passes every other error through.
func TestRejectedWrite(t *testing.T) {
	scheme := testScheme(t)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "default"}}
	gr := schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}
	forbidden := apierrors.NewForbidden(gr, "n", errors.New("exceeded quota"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Kind: "NetworkPolicy"}, "n",
		field.ErrorList{field.Required(field.NewPath("spec"), "")})
	badRequest := apierrors.NewBadRequest("bad policy")
	for _, tc := range []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"forbidden", forbidden, apierrors.IsForbidden},
		{"invalid", invalid, apierrors.IsInvalid},
		{"bad request", badRequest, apierrors.IsBadRequest},
	} {
		got := rejectedWrite("creating", scheme, np, tc.err)
		cr, ok := asChildWriteRejected(got)
		if !ok {
			t.Fatalf("%s: rejectedWrite = %v, want a ChildWriteRejectedError", tc.name, got)
		}
		if cr.Op != "creating" || cr.Kind != "NetworkPolicy" || cr.Name != "n" {
			t.Errorf("%s: error = %+v, want creating NetworkPolicy n", tc.name, cr)
		}
		if want := `creating NetworkPolicy "n": ` + tc.err.Error(); got.Error() != want {
			t.Errorf("%s: message = %q, want %q", tc.name, got.Error(), want)
		}
		if !tc.check(got) {
			t.Errorf("%s: the API error is not reachable through Unwrap", tc.name)
		}
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", apierrors.NewTimeoutError("timeout", 1)},
		{"webhook unreachable", apierrors.NewInternalError(errors.New("connection refused"))},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1)},
		{"conflict", apierrors.NewConflict(gr, "n", errors.New("conflict"))},
		{"plain error", errors.New("dial tcp: connection refused")},
		{"nil", nil},
	} {
		if got := rejectedWrite("updating", scheme, np, tc.err); got != tc.err { //nolint:errorlint // identity check
			t.Errorf("%s: rejectedWrite = %v, want the error unchanged", tc.name, got)
		}
	}
}
