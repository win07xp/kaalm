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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/secretwatch"
)

// A Running Agent with idle detection off that reads a pull or env Secret
// requeues within the watcher's idle TTL, so the Secret's watch stays alive
// between passes. An Agent that reads no Secret keeps the empty result.
func TestAgent_SecretReadsKeepTheWatchAlive(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, class, agent string)
		keepAlive bool
	}{
		{
			name: "env",
			setup: func(t *testing.T, class, agent string) {
				mkEnvSecret(t, agent+"-env", true)
				mkWorkloadClass(t, class, nil)
				mkWorkloadAgent(t, agent, class, func(ag *kaalmv1beta1.Agent) {
					ag.Spec.Env = []corev1.EnvVar{envFromSecret("TOKEN", agent+"-env", false)}
				})
			},
			keepAlive: true,
		},
		{
			name: "pull",
			setup: func(t *testing.T, class, agent string) {
				sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent + "-creds", Namespace: "default"}}
				if err := testClient.Create(ctxT(), sec); err != nil {
					t.Fatalf("create secret: %v", err)
				}
				mkWorkloadClass(t, class, func(ac *kaalmv1beta1.AgentClass) {
					ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: agent + "-creds"}}
				})
				mkWorkloadAgent(t, agent, class, nil)
			},
			keepAlive: true,
		},
		{
			name: "none",
			setup: func(t *testing.T, class, agent string) {
				mkWorkloadClass(t, class, nil)
				mkWorkloadAgent(t, agent, class, nil)
			},
			keepAlive: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, agent := "wc-keepalive-"+tc.name, "keepalive-"+tc.name
			tc.setup(t, class, agent)
			markCertReady(t, agent)
			eventually(t, func() error {
				if agentPod(t, agent) == nil {
					return errString("no pod yet")
				}
				return nil
			})
			markPodReady(t, agentPod(t, agent))
			expectAgentPhase(t, agent, kaalmv1beta1.AgentRunning)

			r := &AgentReconciler{
				Client: testClient, Recorder: events.NewFakeRecorder(10),
				OperatorNamespace: testSystemNamespace, SecretReader: testClient,
				FQDNSupport: fqdnSupportedInTests,
			}
			res, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: agent}})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if !tc.keepAlive {
				if res.RequeueAfter != 0 {
					t.Errorf("an Agent that reads no Secret requeues after %s, want no requeue", res.RequeueAfter)
				}
				return
			}
			if res.RequeueAfter < secretKeepAlive(0) || res.RequeueAfter > secretKeepAlive(1) {
				t.Errorf("RequeueAfter = %s, want between %s and %s so the Secret watch stays alive",
					res.RequeueAfter, secretKeepAlive(0), secretKeepAlive(1))
			}
		})
	}
}

func TestSecretKeepAlive_Jitter(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		want time.Duration
	}{{0, 27 * time.Minute}, {0.5, 30 * time.Minute}, {1, 33 * time.Minute}} {
		if got := secretKeepAlive(tc.u); got != tc.want {
			t.Errorf("secretKeepAlive(%v) = %s, want %s", tc.u, got, tc.want)
		}
	}
}

func TestWithSecretKeepAlive_KeepsSoonerRequeue(t *testing.T) {
	const u = 0.5
	if got := withSecretKeepAlive(ctrl.Result{}, u).RequeueAfter; got != secretKeepAlive(u) {
		t.Errorf("empty result: RequeueAfter = %s, want %s", got, secretKeepAlive(u))
	}
	if got := withSecretKeepAlive(ctrl.Result{RequeueAfter: activityCacheWindow}, u).RequeueAfter; got != activityCacheWindow {
		t.Errorf("sooner requeue: RequeueAfter = %s, want %s kept", got, activityCacheWindow)
	}
	if got := withSecretKeepAlive(ctrl.Result{RequeueAfter: 2 * time.Hour}, u).RequeueAfter; got != secretKeepAlive(u) {
		t.Errorf("later requeue: RequeueAfter = %s, want %s", got, secretKeepAlive(u))
	}
}

func TestReadsSecrets(t *testing.T) {
	literal := corev1.EnvVar{Name: "A", Value: "b"}
	unnamed := envFromSecret("B", "", false)
	for _, tc := range []struct {
		name string
		eff  effectiveAgentSpec
		want bool
	}{
		{"no references", effectiveAgentSpec{}, false},
		{"literal env", effectiveAgentSpec{Env: []corev1.EnvVar{literal}}, false},
		{"secretKeyRef with no name", effectiveAgentSpec{Env: []corev1.EnvVar{unnamed}}, false},
		{"pull Secret", effectiveAgentSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "creds"}}}, true},
		{"env Secret", effectiveAgentSpec{Env: []corev1.EnvVar{literal, envFromSecret("C", "s", false)}}, true},
	} {
		if got := readsSecrets(tc.eff); got != tc.want {
			t.Errorf("%s: readsSecrets = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The keep-alive requeue, jitter included, must stay below the watcher's
// default idle TTL, the one the manager runs with.
func TestSecretKeepAlive_StaysBelowWatcherIdleTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := secretwatch.New(ctx, kubefake.NewSimpleClientset())
	if longest := secretKeepAlive(1); longest >= w.IdleTTL {
		t.Fatalf("the longest keep-alive requeue (%s) reaches the watcher's default IdleTTL (%s): "+
			"a Running Agent's Secret watches would stop between passes", longest, w.IdleTTL)
	}
}
