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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// withReferrerIndexes registers every referrer index SetupIndexers registers,
// from the same table, so a fake client's referrer lookups follow the
// manager's rules. b must already have its scheme set. The fake builder
// panics on a field registered twice, so a caller must not also register a
// referrer index by hand.
func withReferrerIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	for _, r := range referrerFieldIndexes {
		b = b.WithIndex(r.obj, r.f.key, r.f.extract)
	}
	return b
}

func TestEnvSecretRefIndex(t *testing.T) {
	env := []corev1.EnvVar{
		envFromSecret("Z", "zz-creds", false),
		envFromSecret("A", "aa-creds", true),
		envFromSecret("A2", "aa-creds", false),
		envFromSecret("EMPTY", "", false),
		{Name: "PLAIN", Value: "v"},
		{Name: "CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "a-configmap"}, Key: "k"}}},
	}
	want := []string{"aa-creds", "zz-creds"}
	for _, obj := range []client.Object{
		&kaalmv1beta1.Agent{Spec: kaalmv1beta1.AgentSpec{Env: env}},
		&kaalmv1beta1.AgentTask{Spec: kaalmv1beta1.AgentTaskSpec{Env: env}},
	} {
		if got := envSecretRefIndex(obj); !reflect.DeepEqual(got, want) {
			t.Errorf("%T: envSecretRefIndex = %v, want %v", obj, got, want)
		}
	}
	if got := envSecretRefIndex(&kaalmv1beta1.Agent{}); len(got) != 0 {
		t.Errorf("an Agent with no env indexed %v", got)
	}
	if got := envSecretRefIndex(&kaalmv1beta1.AgentClass{}); got != nil {
		t.Errorf("a non-workload object indexed %v", got)
	}
}

func TestClassPullSecretIndex(t *testing.T) {
	ac := &kaalmv1beta1.AgentClass{Spec: kaalmv1beta1.AgentClassSpec{Image: kaalmv1beta1.AgentClassImage{
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "zz-creds"}, {Name: "aa-creds"}},
	}}}
	if got, want := classPullSecretIndex(ac), []string{"zz-creds", "aa-creds"}; !reflect.DeepEqual(got, want) {
		t.Errorf("classPullSecretIndex = %v, want %v", got, want)
	}
	if got := classPullSecretIndex(&kaalmv1beta1.AgentClass{}); len(got) != 0 {
		t.Errorf("a class with no pull Secrets indexed %v", got)
	}
	if got := classPullSecretIndex(&kaalmv1beta1.Agent{}); got != nil {
		t.Errorf("a non-class object indexed %v", got)
	}
}
