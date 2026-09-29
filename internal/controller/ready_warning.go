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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// readyFalseIsNew reports whether setting Ready=False with reason would be a
// change from conds: there is no Ready condition, it is not False, or it
// carries another reason. The reconcilers emit a reconcile-time validation
// failure as a Warning event only then, so a resource that fails the same
// check on every pass (or every gateRequeue) produces one event, not one per
// pass.
func readyFalseIsNew(conds []metav1.Condition, reason string) bool {
	prev := apimeta.FindStatusCondition(conds, kaalmv1beta1.ConditionReady)
	return prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason
}
