package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func disconnectFixture(t *testing.T, phase kaalmv1beta1.AgentChannelPhase) (*kaalmv1beta1.AgentChannel, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "ch"},
		Status:     kaalmv1beta1.AgentChannelStatus{Phase: phase},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ch).WithStatusSubresource(ch).Build()
	return ch, c
}

func disconnected(t *testing.T, c client.Client) bool {
	t.Helper()
	var got kaalmv1beta1.AgentChannel
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "ch"}, &got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[kaalmv1beta1.AnnotationChannelDisconnected] == kaalmv1beta1.AnnotationTrue
}

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// A replica that starts or re-lists after the phase flip sees the channel
// only through Add; it must still confirm.
func TestChannelDisconnectHandler_AddConfirmsTerminating(t *testing.T) {
	ch, c := disconnectFixture(t, kaalmv1beta1.ChannelTerminating)
	h := channelDisconnectHandler(context.Background(), c, quietLogger)
	h.AddFunc(ch)
	if !disconnected(t, c) {
		t.Fatal("Add of a Terminating channel must set the disconnect annotation")
	}
}

func TestChannelDisconnectHandler_UpdateConfirmsTerminating(t *testing.T) {
	ch, c := disconnectFixture(t, kaalmv1beta1.ChannelTerminating)
	h := channelDisconnectHandler(context.Background(), c, quietLogger)
	h.UpdateFunc(ch, ch)
	if !disconnected(t, c) {
		t.Fatal("Update to Terminating must set the disconnect annotation")
	}
}

func TestChannelDisconnectHandler_IgnoresActive(t *testing.T) {
	ch, c := disconnectFixture(t, kaalmv1beta1.ChannelActive)
	h := channelDisconnectHandler(context.Background(), c, quietLogger)
	h.AddFunc(ch)
	h.UpdateFunc(ch, ch)
	if disconnected(t, c) {
		t.Fatal("an Active channel must not be confirmed")
	}
}
