package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayReplicaCount counts the gateway Pods for the rate limiter's
// per-replica share and the hard budget's margin. A Pod being deleted is
// draining and takes no new connections, so it is left out; counting it
// would shrink every serving replica's share during a rollout. Pods not yet
// Ready are counted, which errs toward the smaller share. An error or an
// empty count gives 1.
func gatewayReplicaCount(ctx context.Context, reader client.Reader, namespace string) int {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "gateway"}); err != nil {
		return 1
	}
	n := 0
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			n++
		}
	}
	return max(n, 1)
}
