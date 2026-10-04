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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Only a deliberate refusal of the Pod is a rejection; anything a backoff
// retry can clear stays an error.
func TestIsPodCreateRejection(t *testing.T) {
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
		if got := isPodCreateRejection(tc.err); got != tc.want {
			t.Errorf("%s: isPodCreateRejection(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
