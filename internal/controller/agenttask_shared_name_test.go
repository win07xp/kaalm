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
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// sharedName is the name the Agent and the AgentTask share.
const sharedName = "shared-name"

// expectControlledBy waits for obj, read by name, to be controlled by the
// ownerKind called sharedName.
func expectControlledBy(t *testing.T, obj client.Object, name, ownerKind string) {
	t.Helper()
	eventually(t, func() error {
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
			return err
		}
		if ref := metav1.GetControllerOf(obj); ref == nil || ref.Kind != ownerKind || ref.Name != sharedName {
			return fmt.Errorf("%T %s is controlled by %+v, want %s %s", obj, name, ref, ownerKind, sharedName)
		}
		return nil
	})
}

// An Agent and an AgentTask of the same name both provision: each gets its
// own NetworkPolicy, Certificate, TLS Secret, and CiliumNetworkPolicy, and
// neither waits on a ChildConflict.
func TestAgentAndTask_ShareAName(t *testing.T) {
	const name = sharedName
	mkWorkloadClass(t, "wc-shared-name", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	})
	mkWorkloadAgent(t, name, "wc-shared-name", nil)
	mkTask(t, name, "wc-shared-name", nil)
	eventually(t, func() error { return markCertReadyErr(name) })
	eventually(t, func() error { return markTaskCertReadyErr(name) })

	eventually(t, func() error {
		if agentPod(t, name) == nil {
			return errString("no Agent pod yet")
		}
		if taskPod(t, name) == nil {
			return errString("no task pod yet")
		}
		return nil
	})
	if c := condition(getWorkloadAgent(t, name).Status.Conditions, kaalmv1beta1.ConditionReady); c != nil &&
		c.Reason == kaalmv1beta1.ReasonChildConflict {
		t.Errorf("Agent Ready = %s: %s", c.Reason, c.Message)
	}
	if c := condition(getTask(t, name).Status.Conditions, kaalmv1beta1.ConditionReady); c != nil &&
		c.Reason == kaalmv1beta1.ReasonChildConflict {
		t.Errorf("task Ready = %s: %s", c.Reason, c.Message)
	}

	expectControlledBy(t, &networkingv1.NetworkPolicy{}, name, "Agent")
	expectControlledBy(t, &networkingv1.NetworkPolicy{}, taskNetworkPolicyName(name), "AgentTask")
	expectControlledBy(t, &cmapi.Certificate{}, agentCertificateName(name), "Agent")
	expectControlledBy(t, &cmapi.Certificate{}, taskCertificateName(name), "AgentTask")
	expectFQDNHosts(t, fqdnPolicyName(name), name, "Agent", "api.example.com")
	expectFQDNHosts(t, taskFQDNPolicyName(name), name, "AgentTask", "api.example.com")

	// The two Certificates write different Secrets, whatever the two UIDs.
	var agentCert, taskCert cmapi.Certificate
	for _, c := range []struct {
		name string
		obj  *cmapi.Certificate
	}{{agentCertificateName(name), &agentCert}, {taskCertificateName(name), &taskCert}} {
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: c.name}, c.obj); err != nil {
			t.Fatalf("Certificate %s: %v", c.name, err)
		}
	}
	task := getTask(t, name)
	if want := name + ".task-tls-" + string(task.UID)[:certSecretUIDChars]; taskCert.Spec.SecretName != want {
		t.Errorf("task Secret = %q, want %q", taskCert.Spec.SecretName, want)
	}
	if agentCert.Spec.SecretName == taskCert.Spec.SecretName {
		t.Errorf("the Agent and the task share the Secret %q", taskCert.Spec.SecretName)
	}
}
