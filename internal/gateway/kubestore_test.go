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

package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/secretwatch"
)

// gatewayScheme builds a scheme carrying the core and kaalm types.
func gatewayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// kubeClientWith builds a fake reader seeded with objs, a status.podIP
// index, and the AgentChannel path index.
func kubeClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(gatewayScheme(t)).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, PodIPIndex, func(o client.Object) []string {
			ip := o.(*corev1.Pod).Status.PodIP
			if ip == "" {
				return nil
			}
			return []string{ip}
		}).
		WithIndex(&kaalmv1beta1.AgentChannel{}, ChannelPathIndex, ChannelPathIndexValue).
		Build()
}

func TestKubeStore_AgentTaskClassProvider(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "team-a"}}
	class := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "std"}}
	prov := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "prov"}}

	k := &KubeStore{Reader: kubeClientWith(t, agent, task, class, prov), OperatorNamespace: "kaalm-system"}
	ctx := context.Background()

	if a, ok := k.AgentByName(ctx, "team-a", "sup"); !ok || a.Name != "sup" {
		t.Errorf("AgentByName miss: %v %v", a, ok)
	}
	if _, ok := k.AgentByName(ctx, "team-a", "nope"); ok {
		t.Error("AgentByName should miss unknown agent")
	}
	if tk, ok := k.TaskByName(ctx, "team-a", "fix"); !ok || tk.Name != "fix" {
		t.Errorf("TaskByName miss: %v %v", tk, ok)
	}
	if _, ok := k.TaskByName(ctx, "team-a", "nope"); ok {
		t.Error("TaskByName should miss unknown task")
	}
	if c, ok := k.ClassByName(ctx, "std"); !ok || c.Name != "std" {
		t.Errorf("ClassByName miss: %v %v", c, ok)
	}
	if _, ok := k.ClassByName(ctx, "nope"); ok {
		t.Error("ClassByName should miss unknown class")
	}
	if p, ok := k.ProviderByName(ctx, "prov"); !ok || p.Name != "prov" {
		t.Errorf("ProviderByName miss: %v %v", p, ok)
	}
	if _, ok := k.ProviderByName(ctx, "nope"); ok {
		t.Error("ProviderByName should miss unknown provider")
	}
}

func TestKubeStore_Credential(t *testing.T) {
	prov := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Endpoint:       "https://api.example.com",
			CredentialsRef: kaalmv1beta1.SecretKeyReference{Name: "prov-secret", Key: "api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prov-secret", Namespace: "kaalm-system",
			Labels: providerLabels(), Annotations: providerHosts("api.example.com"),
		},
		Data: map[string][]byte{"api-key": []byte("sk-live")},
	}
	k := &KubeStore{Reader: kubeClientWith(t, prov, secret), OperatorNamespace: "kaalm-system"}
	ctx := context.Background()

	got, err := k.Credential(ctx, prov)
	if err != nil || got != "sk-live" {
		t.Fatalf("Credential = %q err=%v", got, err)
	}

	// Missing key in the Secret.
	provBadKey := prov.DeepCopy()
	provBadKey.Spec.CredentialsRef.Key = "absent"
	if _, err := k.Credential(ctx, provBadKey); err == nil {
		t.Error("missing key must error")
	}

	// Missing Secret entirely.
	provNoSecret := prov.DeepCopy()
	provNoSecret.Spec.CredentialsRef.Name = "ghost"
	if _, err := k.Credential(ctx, provNoSecret); err == nil {
		t.Error("missing Secret must error")
	}

	// Empty value is treated as missing.
	emptySec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "empty", Namespace: "kaalm-system",
			Labels: providerLabels(), Annotations: providerHosts("api.example.com"),
		},
		Data: map[string][]byte{"api-key": {}},
	}
	k2 := &KubeStore{Reader: kubeClientWith(t, emptySec), OperatorNamespace: "kaalm-system"}
	provEmpty := prov.DeepCopy()
	provEmpty.Spec.CredentialsRef.Name = "empty"
	if _, err := k2.Credential(ctx, provEmpty); err == nil {
		t.Error("empty credential value must error")
	}
}

// channelSecret builds a Secret in team-a with a token key.
func channelSecret(name string, labels, annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels, Annotations: annotations},
		Data:       map[string][]byte{"token": []byte("hunter2")},
	}
}

func optInLabels(value string) map[string]string {
	return map[string]string{kaalmv1beta1.LabelChannelCredential: value}
}

func TestKubeStore_SecretValue(t *testing.T) {
	k := &KubeStore{Reader: kubeClientWith(t,
		channelSecret("chan-secret", optInLabels("true"), nil),
		channelSecret("unlabeled", nil, nil),
		channelSecret("label-True", optInLabels("True"), nil),
		channelSecret("label-false", optInLabels("false"), nil),
	), OperatorNamespace: "kaalm-system"}
	ctx := context.Background()

	if v, err := k.SecretValue(ctx, "team-a", "chan-secret", "token"); err != nil || v != "hunter2" {
		t.Fatalf("SecretValue = %q err=%v", v, err)
	}
	if _, err := k.SecretValue(ctx, "team-a", "chan-secret", "absent"); err == nil {
		t.Error("missing key must error")
	}
	if _, err := k.SecretValue(ctx, "team-a", "ghost", "token"); err == nil {
		t.Error("missing Secret must error")
	}
	// Rule 45: a Secret that did not opt in is refused even when readable.
	for _, name := range []string{"unlabeled", "label-True", "label-false"} {
		v, err := k.SecretValue(ctx, "team-a", name, "token")
		if err == nil || v != "" {
			t.Errorf("%s: SecretValue = %q err=%v, want a refusal", name, v, err)
		}
		if err != nil && !strings.Contains(err.Error(), kaalmv1beta1.LabelChannelCredential) {
			t.Errorf("%s: error %q does not name the label", name, err)
		}
	}
}

// Rule 46: a bearer callback token is returned only for a host its Secret
// lists, on top of the rule 45 label.
func TestKubeStore_CallbackSecretValue(t *testing.T) {
	hosts := map[string]string{kaalmv1beta1.AnnotationCallbackHosts: "a.example.com, Hooks.Example.com"}
	k := &KubeStore{Reader: kubeClientWith(t,
		channelSecret("cb", optInLabels("true"), hosts),
		channelSecret("cb-nohosts", optInLabels("true"), nil),
		channelSecret("cb-unlabeled", nil, hosts),
	), OperatorNamespace: "kaalm-system"}
	ctx := context.Background()

	if v, err := k.CallbackSecretValue(ctx, "team-a", "cb", "token", "hooks.example.com"); err != nil || v != "hunter2" {
		t.Fatalf("approved host: %q err=%v", v, err)
	}
	refused := []struct{ name, host string }{
		{"cb", "evil.example.com"},
		{"cb", "example.com"},
		{"cb", ""},
		{"cb-nohosts", "hooks.example.com"},
		{"cb-unlabeled", "hooks.example.com"},
		{"ghost", "hooks.example.com"},
	}
	for _, c := range refused {
		if v, err := k.CallbackSecretValue(ctx, "team-a", c.name, "token", c.host); err == nil || v != "" {
			t.Errorf("%s for %q: %q err=%v, want a refusal", c.name, c.host, v, err)
		}
	}
	if _, err := k.CallbackSecretValue(ctx, "team-a", "cb", "absent", "hooks.example.com"); err == nil {
		t.Error("missing key must error")
	}
}

// readyChannel builds an AgentChannel at path, optionally Ready.
func readyChannel(ns, name, path string, ready bool) *kaalmv1beta1.AgentChannel {
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kaalmv1beta1.AgentChannelSpec{
			Webhook: &kaalmv1beta1.AgentChannelWebhook{Path: path},
		},
	}
	if ready {
		ch.Status.Conditions = []metav1.Condition{
			{Type: kaalmv1beta1.ConditionReady, Status: "True", Reason: "Ready",
				LastTransitionTime: metav1.Now()},
		}
	}
	return ch
}

func TestKubeStore_ChannelByPath(t *testing.T) {
	ctx := context.Background()

	ready := readyChannel("team-a", "ch", "/channels/team-a/hook", true)
	notReady := readyChannel("team-b", "cb", "/channels/team-b/hook2", false)
	// Path does not begin with the channel's own namespace prefix (rule 15).
	spoofed := readyChannel("team-c", "cc", "/channels/team-a/spoof", true)

	k := &KubeStore{Reader: kubeClientWith(t, ready, notReady, spoofed), OperatorNamespace: "kaalm-system"}

	if ch, ok := k.ChannelByPath(ctx, "/channels/team-a/hook"); !ok || ch.Name != "ch" {
		t.Errorf("Ready channel not found: %v %v", ch, ok)
	}
	if _, ok := k.ChannelByPath(ctx, "/channels/team-b/hook2"); ok {
		t.Error("non-Ready channel must not resolve")
	}
	if _, ok := k.ChannelByPath(ctx, "/channels/team-a/spoof"); ok {
		t.Error("channel whose path escapes its namespace prefix must be rejected")
	}
	if _, ok := k.ChannelByPath(ctx, "/channels/team-a/unknown"); ok {
		t.Error("unknown path must miss")
	}
}

// TestKubeStore_ChannelByPathPicksRule15Winner: when two Ready channels share
// a path (the loser's status has not caught up yet), the gateway routes to
// rule 15's winner, the earlier creationTimestamp with a tie to the lower
// name, whatever order the cache lists them in (#326).
func TestKubeStore_ChannelByPathPicksRule15Winner(t *testing.T) {
	ctx := context.Background()
	t0 := metav1.NewTime(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Second))
	const path = "/channels/team-a/shared"
	at := func(name string, created metav1.Time, ready bool) *kaalmv1beta1.AgentChannel {
		ch := readyChannel("team-a", name, path, ready)
		ch.CreationTimestamp = created
		return ch
	}
	cases := []struct {
		name string
		objs []client.Object
		want string
	}{
		{"older sorts first", []client.Object{at("ch-a", t0, true), at("ch-b", t1, true)}, "ch-a"},
		{"older sorts last", []client.Object{at("ch-a", t1, true), at("ch-b", t0, true)}, "ch-b"},
		{"tie goes to the lower name", []client.Object{at("ch-b", t0, true), at("ch-a", t0, true)}, "ch-a"},
		{"a non-Ready older channel is skipped", []client.Object{at("ch-a", t0, false), at("ch-b", t1, true)}, "ch-b"},
		{"three Ready channels", []client.Object{
			at("ch-c", t1, true), at("ch-b", t0, true), at("ch-a", t1, true)}, "ch-b"},
	}
	for _, tc := range cases {
		k := &KubeStore{Reader: kubeClientWith(t, tc.objs...), OperatorNamespace: "kaalm-system"}
		ch, ok := k.ChannelByPath(ctx, path)
		if !ok || ch.Name != tc.want {
			name := ""
			if ch != nil {
				name = ch.Name
			}
			t.Errorf("%s: ChannelByPath = %q ok=%v, want %q", tc.name, name, ok, tc.want)
		}
	}
}

func TestChannelPathAllowed(t *testing.T) {
	ok := readyChannel("team-a", "ch", "/channels/team-a/x", false)
	if !channelPathAllowed(ok) {
		t.Error("in-namespace prefix must be allowed")
	}
	bad := readyChannel("team-a", "ch", "/channels/team-b/x", false)
	if channelPathAllowed(bad) {
		t.Error("cross-namespace prefix must be rejected")
	}
}

func TestKubeStore_PodByIP(t *testing.T) {
	ctx := context.Background()

	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sup-abc", Namespace: "team-a"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.5", Phase: corev1.PodRunning},
	}
	// A terminated Pod sharing a recycled IP must be skipped.
	dead := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "team-a"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.9", Phase: corev1.PodSucceeded},
	}
	k := &KubeStore{Reader: kubeClientWith(t, running, dead), OperatorNamespace: "kaalm-system"}

	if p, ok := k.PodByIP(ctx, "10.0.0.5"); !ok || p.Name != "sup-abc" {
		t.Errorf("PodByIP hit failed: %v %v", p, ok)
	}
	if _, ok := k.PodByIP(ctx, "10.0.0.9"); ok {
		t.Error("terminated Pod must be skipped")
	}
	if _, ok := k.PodByIP(ctx, "10.0.0.250"); ok {
		t.Error("unknown IP must miss")
	}

	// The cached lookup asks for the cache's own object rather than a deep
	// copy: one copy per request was a quarter of the gateway's allocations.
	rec := &recordingReader{Reader: k.Reader}
	k.Reader = rec
	if _, ok := k.PodByIP(ctx, "10.0.0.5"); !ok {
		t.Fatal("hit through the recording reader failed")
	}
	if rec.last.UnsafeDisableDeepCopy == nil || !*rec.last.UnsafeDisableDeepCopy {
		t.Error("PodByIP must list with UnsafeDisableDeepCopy")
	}
}

// recordingReader captures the options of the last List it forwarded.
type recordingReader struct {
	client.Reader
	last client.ListOptions
}

func (r *recordingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.last = client.ListOptions{}
	r.last.ApplyOptions(opts)
	return r.Reader.List(ctx, list, opts...)
}

func TestKubeStore_PodByIPLive(t *testing.T) {
	ctx := context.Background()

	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-pod", Namespace: "team-a"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.5", Phase: corev1.PodRunning},
	}
	dead := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "team-a"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.9", Phase: corev1.PodFailed},
	}
	live := kubeClientWith(t, running, dead)

	// Nil APIReader disables the fallback.
	k := &KubeStore{Reader: kubeClientWith(t), OperatorNamespace: "kaalm-system"}
	if _, ok := k.PodByIPLive(ctx, "team-a", "10.0.0.5"); ok {
		t.Error("nil APIReader must miss")
	}

	k = &KubeStore{Reader: kubeClientWith(t), APIReader: live, OperatorNamespace: "kaalm-system"}
	if p, ok := k.PodByIPLive(ctx, "team-a", "10.0.0.5"); !ok || p.Name != "fix-pod" {
		t.Errorf("PodByIPLive hit failed: %v %v", p, ok)
	}
	// The query is namespace-narrowed: the same IP misses for another
	// namespace.
	if _, ok := k.PodByIPLive(ctx, "team-b", "10.0.0.5"); ok {
		t.Error("live lookup must not match across namespaces")
	}
	if _, ok := k.PodByIPLive(ctx, "team-a", "10.0.0.9"); ok {
		t.Error("terminated Pod must be skipped")
	}
	if _, ok := k.PodByIPLive(ctx, "team-a", "10.0.0.250"); ok {
		t.Error("unknown IP must miss")
	}
}

// secretObj builds a Secret with one "token" key.
func secretObj(ns, name, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"token": []byte(value)},
	}
}

// optedIn adds the rule 45 label a channel credential Secret carries.
func optedIn(sec *corev1.Secret) *corev1.Secret {
	sec.Labels = optInLabels(kaalmv1beta1.AnnotationTrue)
	return sec
}

// providerOptedIn adds the rule 49 label and the rule 50 host list a
// provider credential Secret carries.
func providerOptedIn(sec *corev1.Secret, hosts string) *corev1.Secret {
	sec.Labels = providerLabels()
	sec.Annotations = providerHosts(hosts)
	return sec
}

func TestKubeStoreReadsSecretsThroughTheWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := kubefake.NewSimpleClientset(
		providerOptedIn(secretObj("kaalm-system", "openai-key", "sk-live"), "api.openai.com"),
		providerOptedIn(secretObj("kaalm-system", "tool-key", "tool-live"), "mcp.example.com"),
		optedIn(secretObj("team-a", "hook", "hook-live")),
	)
	store := &KubeStore{
		Reader:            kubeClientWith(t),
		OperatorNamespace: "kaalm-system",
		Secrets:           secretwatch.New(ctx, cs),
	}
	provider := &kaalmv1beta1.ModelProvider{Spec: kaalmv1beta1.ModelProviderSpec{
		Endpoint:       "https://api.openai.com",
		CredentialsRef: kaalmv1beta1.SecretKeyReference{Name: "openai-key", Key: "token"},
	}}
	if got, err := store.Credential(ctx, provider); err != nil || got != "sk-live" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	tool := &kaalmv1beta1.ToolProvider{Spec: kaalmv1beta1.ToolProviderSpec{
		Endpoint:       "https://mcp.example.com",
		CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: "tool-key", Key: "token"},
	}}
	if got, err := store.ToolCredential(ctx, tool); err != nil || got != "tool-live" {
		t.Fatalf("ToolCredential = %q, %v", got, err)
	}
	if got, err := store.SecretValue(ctx, "team-a", "hook", "token"); err != nil || got != "hook-live" {
		t.Fatalf("SecretValue = %q, %v", got, err)
	}
	if _, err := store.SecretValue(ctx, "team-a", "hook", "missing"); err == nil {
		t.Fatal("missing key must error")
	}
}

// providerCredSecret builds a kaalm-system Secret with a "token" key and the
// given labels and annotations.
func providerCredSecret(name string, labels, annotations map[string]string, data map[string][]byte) *corev1.Secret {
	if data == nil {
		data = map[string][]byte{"token": []byte("sk-secret-value")}
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kaalm-system", Labels: labels, Annotations: annotations,
		},
		Data: data,
	}
}

func providerLabels() map[string]string {
	return map[string]string{kaalmv1beta1.LabelProviderCredential: kaalmv1beta1.AnnotationTrue}
}

func providerHosts(hosts string) map[string]string {
	return map[string]string{kaalmv1beta1.AnnotationProviderHosts: hosts}
}

// providerCredentialReaders returns a Credential reader and a ToolCredential
// reader for a provider at endpoint whose credentialsRef names the "token"
// key of secret.
func providerCredentialReaders(k *KubeStore, endpoint, secret string) map[string]func() (string, error) {
	const key = "token"
	ctx := context.Background()
	mp := &kaalmv1beta1.ModelProvider{Spec: kaalmv1beta1.ModelProviderSpec{
		Endpoint: endpoint, CredentialsRef: kaalmv1beta1.SecretKeyReference{Name: secret, Key: key},
	}}
	tp := &kaalmv1beta1.ToolProvider{Spec: kaalmv1beta1.ToolProviderSpec{
		Endpoint: endpoint, CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: secret, Key: key},
	}}
	return map[string]func() (string, error){
		"Credential":     func() (string, error) { return k.Credential(ctx, mp) },
		"ToolCredential": func() (string, error) { return k.ToolCredential(ctx, tp) },
	}
}

// Rule 49: the gateway refuses a provider credential Secret without the
// kaalm.io/provider-credential label, before it looks at any key, and the
// error never carries the value.
func TestKubeStore_CredentialRefusesUnlabeledSecret(t *testing.T) {
	hosts := providerHosts("api.example.com")
	k := &KubeStore{Reader: kubeClientWith(t,
		providerCredSecret("unlabeled", nil, hosts, nil),
		providerCredSecret("unlabeled-nokey", nil, hosts, map[string][]byte{}),
		providerCredSecret("label-True", map[string]string{kaalmv1beta1.LabelProviderCredential: "True"}, hosts, nil),
		providerCredSecret("channel-only", optInLabels("true"), hosts, nil),
	), OperatorNamespace: "kaalm-system"}
	for _, name := range []string{"unlabeled", "unlabeled-nokey", "label-True", "channel-only"} {
		for method, read := range providerCredentialReaders(k, "https://api.example.com", name) {
			v, err := read()
			if err == nil || v != "" {
				t.Fatalf("%s %s: %q err=%v, want a refusal", method, name, v, err)
			}
			if !strings.Contains(err.Error(), kaalmv1beta1.LabelProviderCredential) {
				t.Errorf("%s %s: error %q does not name the label", method, name, err)
			}
			if strings.Contains(err.Error(), "sk-secret-value") || strings.Contains(err.Error(), "token") {
				t.Errorf("%s %s: error %q carries the value or a key", method, name, err)
			}
		}
	}
}

// Rule 50: the gateway returns a provider credential only for an endpoint
// host its Secret lists in kaalm.io/provider-hosts.
func TestKubeStore_CredentialRefusesUnapprovedEndpointHost(t *testing.T) {
	k := &KubeStore{Reader: kubeClientWith(t,
		providerCredSecret("approved", providerLabels(), providerHosts("other.example.com, API.example.com"), nil),
		providerCredSecret("no-hosts", providerLabels(), nil, nil),
		providerCredSecret("other-host", providerLabels(), providerHosts("other.example.com"), nil),
		providerCredSecret("callback-only", providerLabels(),
			map[string]string{kaalmv1beta1.AnnotationCallbackHosts: "api.example.com"}, nil),
	), OperatorNamespace: "kaalm-system"}

	for _, endpoint := range []string{"https://api.example.com", "https://api.example.com:8443/v1"} {
		for method, read := range providerCredentialReaders(k, endpoint, "approved") {
			if v, err := read(); err != nil || v != "sk-secret-value" {
				t.Fatalf("%s at %s: %q err=%v, want the value", method, endpoint, v, err)
			}
		}
	}
	refused := []struct{ secret, endpoint string }{
		{"no-hosts", "https://api.example.com"},
		{"other-host", "https://api.example.com"},
		{"callback-only", "https://api.example.com"},
		{"approved", "https://evil.example.com"},
		{"approved", "https://"},
	}
	for _, c := range refused {
		for method, read := range providerCredentialReaders(k, c.endpoint, c.secret) {
			v, err := read()
			if err == nil || v != "" {
				t.Fatalf("%s %s at %s: %q err=%v, want a refusal", method, c.secret, c.endpoint, v, err)
			}
			if !strings.Contains(err.Error(), kaalmv1beta1.AnnotationProviderHosts) {
				t.Errorf("%s %s: error %q does not name the annotation", method, c.secret, err)
			}
			if strings.Contains(err.Error(), "sk-secret-value") {
				t.Errorf("%s %s: error %q carries the value", method, c.secret, err)
			}
		}
	}
	// The host error names the host it refused.
	_, err := providerCredentialReaders(k, "https://evil.example.com", "approved")["Credential"]()
	if err == nil || !strings.Contains(err.Error(), `"evil.example.com"`) {
		t.Errorf("error %v does not name the endpoint host", err)
	}

	// A ToolProvider without credentialsRef reads no Secret.
	if v, err := k.ToolCredential(context.Background(), &kaalmv1beta1.ToolProvider{}); err != nil || v != "" {
		t.Errorf("nil credentialsRef: %q err=%v, want no credential and no error", v, err)
	}
}

// The rule 49 and 50 checks run on every read through the watcher, so a
// label or annotation removed takes effect on the next request.
func TestKubeStoreProviderCredentialFollowsTheSecretThroughTheWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := kubefake.NewSimpleClientset(
		providerCredSecret("live", providerLabels(), providerHosts("api.example.com"), nil),
	)
	store := &KubeStore{
		Reader:            kubeClientWith(t),
		OperatorNamespace: "kaalm-system",
		Secrets:           secretwatch.New(ctx, cs),
	}
	readers := providerCredentialReaders(store, "https://api.example.com", "live")
	for method, read := range readers {
		if v, err := read(); err != nil || v != "sk-secret-value" {
			t.Fatalf("%s = %q, %v", method, v, err)
		}
	}
	update := func(sec *corev1.Secret) {
		t.Helper()
		if _, err := cs.CoreV1().Secrets("kaalm-system").Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(want string) {
		t.Helper()
		for method, read := range readers {
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := read()
				if err != nil && strings.Contains(err.Error(), want) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s never refused with %q (last err %v)", method, want, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}

	update(providerCredSecret("live", providerLabels(), nil, nil))
	waitFor(kaalmv1beta1.AnnotationProviderHosts)

	update(providerCredSecret("live", nil, providerHosts("api.example.com"), nil))
	waitFor(kaalmv1beta1.LabelProviderCredential)
}
