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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// Rule 51: a malformed allowedNamespaces entry longer than the condition
// message limit still turns Ready False. The message quotes only the entry's
// start, so the status write is accepted.
func TestModelProvider_LongNamespacePatternTurnsReadyFalse(t *testing.T) {
	const name = "mp-long-pattern"
	mkSecret(t, name+"-key")
	mkProvider(t, name, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.AllowedNamespaces = []string{"[" + strings.Repeat("a", 40<<10)}
	})
	get := providerConditions(name)
	eventually(t, func() error {
		c := condition(get(), kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonInvalidNamespacePattern {
			return fmt.Errorf("Ready = %+v, want False/%s", c, kaalmv1beta1.ReasonInvalidNamespacePattern)
		}
		if !strings.Contains(c.Message, "(first 256 of 40961 bytes)") || len(c.Message) > conditionMessageLimit {
			return fmt.Errorf("Ready message (%d bytes) does not quote the entry's first 256 bytes", len(c.Message))
		}
		return nil
	})
}
