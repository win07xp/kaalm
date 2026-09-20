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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func providerConditions(name string) func() []metav1.Condition {
	return func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: name}, &mp)
		return mp.Status.Conditions
	}
}

// A provider created before its fallback recovers when the fallback appears.
func TestModelProvider_RecoversWhenFallbackCreatedLater(t *testing.T) {
	mkSecret(t, "mp-late-primary-key")
	mkSecret(t, "mp-late-fallback-key")
	mkProvider(t, "mp-late-primary", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "mp-late-fallback"}}
	})
	expectReady(t, providerConditions("mp-late-primary"), metav1.ConditionFalse, kaalmv1beta1.ReasonFallbackIneligible)

	mkProvider(t, "mp-late-fallback", nil)
	expectReady(t, providerConditions("mp-late-primary"), metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
}

// A provider created before its credential Secret recovers when the Secret
// appears.
func TestModelProvider_RecoversWhenSecretCreatedLater(t *testing.T) {
	mkProvider(t, "mp-late-cred", nil)
	expectReady(t, providerConditions("mp-late-cred"), metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)

	mkSecret(t, "mp-late-cred-key")
	expectReady(t, providerConditions("mp-late-cred"), metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
}
