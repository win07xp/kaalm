package main

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// channelDisconnectHandler is the gateway's half of the channel-delete
// handshake: once a channel is observed Terminating, confirm disconnection
// with the annotation the reconciler waits on. It handles Add as well as
// Update, so a replica that starts or re-lists after the phase flip still
// confirms. The intake handler itself refuses webhook writes to a
// Terminating channel.
func channelDisconnectHandler(
	ctx context.Context, c client.Client, logger *slog.Logger,
) toolscache.ResourceEventHandlerFuncs {
	confirm := func(obj any) {
		ch, ok := obj.(*kaalmv1beta1.AgentChannel)
		if !ok || ch.Status.Phase != kaalmv1beta1.ChannelTerminating {
			return
		}
		if ch.Annotations[kaalmv1beta1.AnnotationChannelDisconnected] == kaalmv1beta1.AnnotationTrue {
			return
		}
		patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
			kaalmv1beta1.AnnotationChannelDisconnected, kaalmv1beta1.AnnotationTrue))
		if err := c.Patch(ctx, ch.DeepCopy(), client.RawPatch(types.MergePatchType, patch)); err != nil {
			logger.Warn("disconnect annotation patch failed", "channel", ch.Name, "error", err)
		}
	}
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    confirm,
		UpdateFunc: func(_, newObj any) { confirm(newObj) },
	}
}
