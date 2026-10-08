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

// The rate limiter's share leaves out Pods being deleted, which take no new
// traffic; the hard budget's margin counts them, because a draining Pod still
// holds in-flight requests and settled spend its peers have not seen.
func TestCountGatewayReplicas(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	other := gatewayPod("controller-0", false)
	other.Labels["app.kubernetes.io/component"] = "controller"
	for _, tc := range []struct {
		name string
		objs []client.Object
		want gatewayReplicas
	}{
		{"two serving", []client.Object{gatewayPod("gw-a", false), gatewayPod("gw-b", false), other},
			gatewayReplicas{serving: 2, all: 2}},
		{"rollout with two draining", []client.Object{
			gatewayPod("gw-a", false), gatewayPod("gw-b", false),
			gatewayPod("gw-old-a", true), gatewayPod("gw-old-b", true),
		}, gatewayReplicas{serving: 2, all: 4}},
		{"only draining", []client.Object{gatewayPod("gw-old-a", true)}, gatewayReplicas{serving: 1, all: 1}},
		{"none", nil, gatewayReplicas{serving: 1, all: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			if got := countGatewayReplicas(context.Background(), c, "kaalm-system"); got != tc.want {
				t.Errorf("countGatewayReplicas = %+v, want %+v", got, tc.want)
			}
		})
	}
}
