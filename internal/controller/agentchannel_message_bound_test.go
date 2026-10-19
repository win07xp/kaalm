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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// quotedInPart is what quotedEntry adds to a 40,000-byte value.
const quotedInPart = "(first 256 of 40000 bytes)"

// labeledChannelSecret is a Secret that opts in to channel use, with data.
func labeledChannelSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{kaalmv1beta1.LabelChannelCredential: kaalmv1beta1.AnnotationTrue},
		},
		Data: data,
	}
}

// bearerChannel is a webhook channel whose inbound bearer auth names
// secretName and key.
func bearerChannel(secretName, key string) *kaalmv1beta1.AgentChannel {
	return &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: kaalmv1beta1.AgentChannelSpec{Webhook: &kaalmv1beta1.AgentChannelWebhook{
			Path: "/channels/default/ch",
			Auth: kaalmv1beta1.ChannelAuth{
				Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: secretName, Key: key},
			},
		}},
	}
}

// A Secret name, a key, and the callbackUrl host have no length limit in the
// schema. Each channel Secret message quotes them bounded.
func TestCheckChannelSecrets_LongValuesQuotedInPart(t *testing.T) {
	long := strings.Repeat("a", 40000)
	token := map[string][]byte{"token": []byte("t")}

	callback := bearerChannel("in", "token")
	url := "https://" + long + "/hook"
	callback.Spec.Webhook.CallbackURL = &url
	callback.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
		Type: "bearer", SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "cb", Key: "token"},
	}

	cases := []struct {
		name    string
		channel *kaalmv1beta1.AgentChannel
		reads   map[string]channelSecretRead
	}{
		{name: "Secret missing", channel: bearerChannel(long, "token")},
		{name: "Secret unlabeled", channel: bearerChannel(long, "token"),
			reads: map[string]channelSecretRead{long: {sec: &corev1.Secret{}}}},
		{name: "key missing", channel: bearerChannel("in", long),
			reads: map[string]channelSecretRead{"in": {sec: labeledChannelSecret("in", token)}}},
		{name: "key empty", channel: bearerChannel("in", long),
			reads: map[string]channelSecretRead{"in": {sec: labeledChannelSecret("in", map[string][]byte{long: {}})}}},
		{name: "callback host not approved", channel: callback,
			reads: map[string]channelSecretRead{
				"in": {sec: labeledChannelSecret("in", token)},
				"cb": {sec: labeledChannelSecret("cb", token)},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, msg := checkChannelSecrets(c.channel, c.reads)
			if reason == "" || !strings.Contains(msg, quotedInPart) || len(msg) >= 2048 {
				t.Fatalf("reason %q: message is %d bytes, want one that quotes the value in part", reason, len(msg))
			}
		})
	}
}

func TestValidatePlatformSecret_LongNameQuotedInPart(t *testing.T) {
	long := strings.Repeat("a", 40000)
	notFound := channelSecretRead{err: readOf(nil, long).err}
	bad := func(map[string][]byte) string { return "publicKey is not hex" }
	cases := []struct {
		name  string
		read  channelSecretRead
		shape func(map[string][]byte) string
	}{
		{name: "missing", read: notFound},
		{name: "unlabeled", read: channelSecretRead{sec: &corev1.Secret{}}},
		{name: "key missing", read: channelSecretRead{sec: labeledChannelSecret(long, nil)}},
		{name: "shape", read: channelSecretRead{sec: labeledChannelSecret(long, map[string][]byte{discordKeyPublicKey: []byte("x")})}, shape: bad},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, msg := validatePlatformSecret(c.read, "default", long, []string{discordKeyPublicKey}, c.shape)
			if reason == "" || !strings.Contains(msg, quotedInPart) || len(msg) >= 2048 {
				t.Fatalf("reason %q: message is %d bytes, want one that quotes the name in part", reason, len(msg))
			}
		})
	}
}

// A read error other than NotFound is copied cut.
func TestSecretReadMessage_LongErrorCut(t *testing.T) {
	msg := secretReadMessage(errors.New(strings.Repeat("e", 40000)), "s", "default")
	if len(msg) >= 2048 || !strings.HasSuffix(msg, "... (truncated)") {
		t.Fatalf("message is %d bytes, want under 2048 ending with the truncation marker", len(msg))
	}
}

// expectChannelReadyMessage waits until the channel's Ready condition has
// reason and a message that holds want.
func expectChannelReadyMessage(t *testing.T, name, reason, want string) {
	t.Helper()
	eventually(t, func() error {
		var ch kaalmv1beta1.AgentChannel
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
			return err
		}
		c := condition(ch.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != reason {
			return fmt.Errorf("Ready = %+v, want reason %s", c, reason)
		}
		if !strings.Contains(c.Message, want) {
			return fmt.Errorf("Ready message (%d bytes) does not hold %q", len(c.Message), want)
		}
		return nil
	})
}

// spec.agentRef.name has no length limit in the schema.
func TestChannel_LongAgentRefQuotedInPart(t *testing.T) {
	mkChannelSecret(t, "ch-long-ref-secret")
	mkChannel(t, "ch-long-ref", strings.Repeat("a", 40000), "/channels/default/ch-long-ref", nil)
	expectChannelReadyMessage(t, "ch-long-ref", kaalmv1beta1.ReasonAgentNotFound, quotedInPart)
}

// The path has no length limit in the schema. The conflict message quotes it
// bounded.
func TestChannel_LongPathConflictQuotedInPart(t *testing.T) {
	mkWorkloadClass(t, "chc-long-conf", nil)
	mkWorkloadAgent(t, "ch-agent-long-conf", "chc-long-conf", nil)
	mkChannelSecret(t, "ch-long-conf-a-secret")
	mkChannelSecret(t, "ch-long-conf-b-secret")
	prefix := "/channels/default/"
	path := prefix + strings.Repeat("p", 40000-len(prefix))
	mkChannel(t, "ch-long-conf-a", "ch-agent-long-conf", path, nil)
	expectChannelReady(t, "ch-long-conf-a", metav1.ConditionTrue, "")
	time.Sleep(1100 * time.Millisecond)
	mkChannel(t, "ch-long-conf-b", "ch-agent-long-conf", path, nil)
	loser, _ := expectPathConflict(t, "ch-long-conf-a", "ch-long-conf-b")
	expectChannelReadyMessage(t, loser, kaalmv1beta1.ReasonPathConflict, quotedInPart)
}
