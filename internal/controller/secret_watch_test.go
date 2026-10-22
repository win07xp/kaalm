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
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// fakeSecretChanges stores the function the source subscribes, so a test
// can play the watcher.
type fakeSecretChanges struct{ fn func(types.NamespacedName) }

func (f *fakeSecretChanges) Subscribe(fn func(types.NamespacedName)) { f.fn = fn }

func TestSecretChangeSource(t *testing.T) {
	changes := &fakeSecretChanges{}
	var asked []types.NamespacedName
	src := secretChangeSource(changes, func(_ context.Context, key types.NamespacedName) []reconcile.Request {
		asked = append(asked, key)
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: key.Namespace, Name: "a"}},
			{NamespacedName: types.NamespacedName{Namespace: key.Namespace, Name: "b"}},
		}
	})
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	ctx, cancel := context.WithCancel(context.Background())
	if err := src.Start(ctx, q); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if changes.fn == nil {
		t.Fatal("Start did not subscribe")
	}
	key := types.NamespacedName{Namespace: "team-a", Name: "creds"}
	changes.fn(key)
	if q.Len() != 2 {
		t.Errorf("queue holds %d requests after a change, want 2", q.Len())
	}
	if len(asked) != 1 || asked[0] != key {
		t.Errorf("referrers asked for %v, want [%v]", asked, key)
	}

	// After the controller's context ends, a change enqueues nothing.
	cancel()
	changes.fn(key)
	if q.Len() != 2 {
		t.Errorf("queue holds %d requests after a change past cancel, want 2", q.Len())
	}
	if len(asked) != 1 {
		t.Errorf("referrers ran after cancel: %v", asked)
	}
}

func pullSecretClass(name string, secrets ...string) *kaalmv1beta1.AgentClass {
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, s := range secrets {
		ac.Spec.Image.ImagePullSecrets = append(ac.Spec.Image.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	return ac
}

func TestClassesNamingPullSecret(t *testing.T) {
	objs := []client.Object{
		pullSecretClass("one", "shared"),
		pullSecretClass("two", "other", "shared"),
		pullSecretClass("three", "other"),
		pullSecretClass("none"),
	}
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(&kaalmv1beta1.AgentClass{}, IndexClassPullSecret, classPullSecretIndex).
		WithObjects(objs...).Build()
	got, err := classesNamingPullSecret(ctx, c, "shared")
	if err != nil {
		t.Fatalf("classesNamingPullSecret: %v", err)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("classesNamingPullSecret = %v, want [one two]", got)
	}

	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	if _, err := classesNamingPullSecret(ctx, unindexed, "shared"); err == nil {
		t.Errorf("classesNamingPullSecret without the %s index answered; want the indexed lookup", IndexClassPullSecret)
	}
}

// checkWorkloadsForSecret runs a reconciler's Secret map over a fixed set of
// workloads. mk builds one workload of the kind: in namespace ns, of class
// class, its env reading envSecret ("" for none). workload is that kind's
// empty object, for the fake client's indexes. forSecret runs the map on c.
func checkWorkloadsForSecret(
	t *testing.T, workload client.Object,
	mk func(ns, name, class, envSecret string) client.Object,
	forSecret func(c client.Client, key types.NamespacedName) []reconcile.Request,
) {
	t.Helper()
	const ref = "shared"
	objs := []client.Object{
		pullSecretClass("plain"),
		pullSecretClass("pulls-shared", ref),
		pullSecretClass("pulls-other", "other"),
		mk("team-a", "env", "plain", ref),
		mk("team-a", "pull", "pulls-shared", ""),
		mk("team-a", "both", "pulls-shared", ref),
		mk("team-b", "other-ns", "pulls-shared", ref),
		mk("team-a", "unrelated", "pulls-other", "other"),
	}
	key := types.NamespacedName{Namespace: "team-a", Name: ref}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(workload, IndexEnvSecretRef, envSecretRefIndex).
		WithIndex(workload, IndexAgentClassRef, agentClassRefIndex).
		WithIndex(&kaalmv1beta1.AgentClass{}, IndexClassPullSecret, classPullSecretIndex).
		WithObjects(objs...).Build()
	var names []string
	for _, req := range forSecret(c, key) {
		if req.Namespace != "team-a" {
			t.Errorf("enqueued %s outside the Secret's namespace", req)
		}
		names = append(names, req.Name)
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "both,env,pull"; got != want {
		t.Errorf("enqueued %q, want %q (each workload once)", got, want)
	}

	// The lookup goes through the indexes: a cache without them cannot answer.
	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	if reqs := forSecret(unindexed, key); reqs != nil {
		t.Errorf("without the indexes the map answered %v; want the indexed lookup", reqs)
	}
}
