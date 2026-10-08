package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func gatewayPod(name string, terminating bool) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "kaalm-system",
		Labels: map[string]string{"app.kubernetes.io/component": "gateway"},
	}}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		// The fake client keeps a deleting object only while a finalizer
		// holds it, the way a draining Pod stays until its process exits.
		pod.Finalizers = []string{"test/hold"}
	}
	return pod
}

// A Pod being deleted is draining and takes no new traffic, so it must not
// shrink each serving replica's rate-limit share during a rollout.
func TestGatewayReplicaCount_ExcludesTerminatingPods(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	other := gatewayPod("controller-0", false)
	other.Labels["app.kubernetes.io/component"] = "controller"
	for _, tc := range []struct {
		name string
		objs []client.Object
		want int
	}{
		{"two serving", []client.Object{gatewayPod("gw-a", false), gatewayPod("gw-b", false), other}, 2},
		{"rollout with two draining", []client.Object{
			gatewayPod("gw-a", false), gatewayPod("gw-b", false),
			gatewayPod("gw-old-a", true), gatewayPod("gw-old-b", true),
		}, 2},
		{"only draining", []client.Object{gatewayPod("gw-old-a", true)}, 1},
		{"none", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			if got := gatewayReplicaCount(context.Background(), c, "kaalm-system"); got != tc.want {
				t.Errorf("gatewayReplicaCount = %d, want %d", got, tc.want)
			}
		})
	}
}
