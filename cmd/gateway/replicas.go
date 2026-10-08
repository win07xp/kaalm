package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayReplicas is one listing of the gateway Pods, counted two ways for
// the two features that scale by the replica count. Each count is at least 1;
// a listing error gives 1 for both.
type gatewayReplicas struct {
	// serving counts the Pods not being deleted, for the rate limiter's
	// per-replica share. A deleting Pod is draining and takes no new
	// traffic once endpoints update, so counting it would shrink every
	// serving replica's share during a rollout. Pods not yet Ready are
	// counted, which errs toward the smaller share.
	serving int
	// all counts every gateway Pod that exists, deleting ones included, for
	// the hard budget's boundary margin. A draining Pod still holds
	// in-flight requests and settled spend its peers have not seen yet, the
	// two things the margin covers.
	all int
}

// countGatewayReplicas lists the gateway Pods in namespace once and counts
// them both ways.
func countGatewayReplicas(ctx context.Context, reader client.Reader, namespace string) gatewayReplicas {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "gateway"}); err != nil {
		return gatewayReplicas{serving: 1, all: 1}
	}
	serving := 0
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			serving++
		}
	}
	return gatewayReplicas{serving: max(serving, 1), all: max(len(pods.Items), 1)}
}
