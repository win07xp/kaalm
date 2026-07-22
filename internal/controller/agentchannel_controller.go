/*
Copyright 2026.

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
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// disconnectTimeout bounds how long the channel finalizer waits for the
// gateway's disconnect annotation before sweeping anyway. A variable so tests
// can shorten it.
var disconnectTimeout = 30 * time.Second

// Channel-health state values on the wire.
const (
	healthStateSuccess = "success"
	healthStateFailure = "failure"
	healthStateEmpty   = "empty"
)

// ChannelHealthState is one replica's view of one channel path.
type ChannelHealthState struct {
	State     string  `json:"state"` // success | failure | empty
	Reason    *string `json:"reason"`
	LastError *string `json:"lastError"`
	Timestamp *string `json:"timestamp"`
}

// ReplicaChannelHealth is one gateway replica's /v1/channels/health response.
type ReplicaChannelHealth struct {
	StartedAt     time.Time                     `json:"replicaStartedAt"`
	WindowSeconds int                           `json:"windowSeconds"`
	Channels      map[string]ChannelHealthState `json:"channels"`
}

// ChannelHealthClient fans the health query out to the gateway fleet.
type ChannelHealthClient interface {
	NamespaceChannelHealth(ctx context.Context, namespace string) (reachable []ReplicaChannelHealth, total int, err error)
}

// AgentChannelReconciler validates channels, scopes credential access, reports
// status, and coordinates the delete handshake. It owns no Pods. See
// docs/src/controller/reconcilers.md (AgentChannelReconciler).
type AgentChannelReconciler struct {
	client.Client
	Recorder          record.EventRecorder
	OperatorNamespace string
	// Health polls per-channel gateway delivery health; nil preserves the
	// existing PlatformConnected condition.
	Health ChannelHealthClient
	// AllowPrivateCallbacks relaxes the Rule 22 deny-internal check so a
	// callbackUrl may resolve to a private (RFC1918/ULA) address, for in-cluster
	// or self-hosted receivers. Loopback and link-local (cloud metadata) stay
	// blocked. Must match the gateway's --allow-private-callbacks. Default false.
	AllowPrivateCallbacks bool
}

// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels/finalizers,verbs=update

// Reconcile runs one pass over an AgentChannel.
func (r *AgentChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var channel kaalmv1alpha1.AgentChannel
	if err := r.Get(ctx, req.NamespacedName, &channel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !channel.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &channel)
	}
	if controllerutil.AddFinalizer(&channel, kaalmv1alpha1.ChannelFinalizer) {
		if err := r.Update(ctx, &channel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	channel.Status.ObservedGeneration = channel.Generation

	// The system-namespace guard runs first, as on the workload reconcilers.
	if channel.Namespace == r.OperatorNamespace {
		r.setChannelReady(&channel, false, kaalmv1alpha1.ReasonSystemNamespaceForbidden,
			fmt.Sprintf("AgentChannels may not live in the operator namespace %q", r.OperatorNamespace))
		return ctrl.Result{}, r.Status().Update(ctx, &channel)
	}

	// Step 1: resolve agentRef (an Agent, never an AgentTask).
	var agent kaalmv1alpha1.Agent
	agentErr := r.Get(ctx, types.NamespacedName{Namespace: channel.Namespace, Name: channel.Spec.AgentRef.Name}, &agent)
	if agentErr != nil {
		if !apierrors.IsNotFound(agentErr) {
			return ctrl.Result{}, agentErr
		}
		channel.Status.Phase = kaalmv1alpha1.ChannelFailed
		r.setChannelReady(&channel, false, kaalmv1alpha1.ReasonAgentNotFound,
			fmt.Sprintf("Agent %q not found in namespace %q", channel.Spec.AgentRef.Name, channel.Namespace))
		return ctrl.Result{}, r.Status().Update(ctx, &channel)
	}

	// Steps 2 and 3 validation chain; the first failure reports and stops.
	if reason, msg := r.validateChannel(ctx, &channel, &agent); reason != "" {
		r.setChannelReady(&channel, false, reason, msg)
		r.reducePhase(&channel, &agent)
		return ctrl.Result{}, r.Status().Update(ctx, &channel)
	}
	r.setChannelReady(&channel, true, kaalmv1alpha1.ReasonAgentReachable, "channel is valid")

	// Step 4: channel health poll and the tri-state reduction.
	if r.Health != nil {
		r.reduceChannelHealth(ctx, &channel)
	}

	// Step 5: phase reduction from the Agent's phase.
	r.reducePhase(&channel, &agent)

	// Step 6: prune expired async response ConfigMaps for this channel.
	if err := r.pruneAsyncConfigMaps(ctx, &channel, false); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Status().Update(ctx, &channel); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled AgentChannel", "phase", channel.Status.Phase)
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// validateChannel runs steps 2 and 3: service enabled, path shape, path
// conflict, the per-channel credential Role, and Secret validation. Returns a
// non-empty reason on the first failure.
func (r *AgentChannelReconciler) validateChannel(
	ctx context.Context, channel *kaalmv1alpha1.AgentChannel, agent *kaalmv1alpha1.Agent,
) (string, string) {
	// Step 2: the Agent must expose a Service (delivery target).
	if agent.Spec.Service != nil && !agent.Spec.Service.Enabled {
		return kaalmv1alpha1.ReasonAgentServiceDisabled,
			fmt.Sprintf("Agent %q has service.enabled=false; channels need a delivery target", agent.Name)
	}
	// Rule 15: the path must begin with /channels/{namespace}/. CRD CEL
	// cannot read metadata.namespace, so this lives here.
	prefix := "/channels/" + channel.Namespace + "/"
	if !strings.HasPrefix(channel.Spec.Webhook.Path, prefix) {
		return kaalmv1alpha1.ReasonInvalidPath,
			fmt.Sprintf("webhook.path must begin with %q", prefix)
	}
	// Path conflict: the earliest creationTimestamp wins.
	var channels kaalmv1alpha1.AgentChannelList
	if err := r.List(ctx, &channels, client.InNamespace(channel.Namespace)); err == nil {
		for i := range channels.Items {
			other := &channels.Items[i]
			if other.Name == channel.Name || other.Spec.Webhook.Path != channel.Spec.Webhook.Path {
				continue
			}
			if other.CreationTimestamp.Before(&channel.CreationTimestamp) ||
				(other.CreationTimestamp.Equal(&channel.CreationTimestamp) && other.Name < channel.Name) {
				return kaalmv1alpha1.ReasonPathConflict,
					fmt.Sprintf("path %q is already registered by the older channel %q",
						channel.Spec.Webhook.Path, other.Name)
			}
		}
	}
	// Step 3: the scoped Role must exist BEFORE any Secret read: it is what
	// grants the reconciler (and the gateway) access to exactly these Secrets.
	if err := r.ensureCredentialRole(ctx, channel); err != nil {
		return kaalmv1alpha1.ReasonInvalidReference, "ensuring the credential Role failed: " + err.Error()
	}
	if reason, msg := r.validateSecrets(ctx, channel); reason != "" {
		return reason, msg
	}
	// Rule 22: callbackUrl must be HTTPS and must not point into internal
	// address space (reconcile-time half; the gateway re-checks pre-dial).
	if channel.Spec.Webhook.CallbackURL != nil {
		if reason, msg := validateCallbackURL(*channel.Spec.Webhook.CallbackURL, r.AllowPrivateCallbacks); reason != "" {
			return reason, msg
		}
	}
	return "", ""
}

// authSecretNames collects the Secret names the channel's auth config
// references (inbound always; callbackAuth when callbackUrl is set).
func authSecretNames(channel *kaalmv1alpha1.AgentChannel) []string {
	set := map[string]bool{}
	collect := func(auth *kaalmv1alpha1.ChannelAuth) {
		if auth == nil {
			return
		}
		if auth.SecretRef != nil {
			set[auth.SecretRef.Name] = true
		}
		if auth.HMAC != nil {
			set[auth.HMAC.SecretRef.Name] = true
		}
	}
	collect(&channel.Spec.Webhook.Auth)
	if channel.Spec.Webhook.CallbackURL != nil {
		collect(channel.Spec.Webhook.CallbackAuth)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func channelRoleName(channelName string) string { return "kaalm-channel-" + channelName + "-creds" }

// ensureCredentialRole creates or updates the per-channel Role (get, watch,
// resourceNames-scoped; list deliberately omitted since resourceNames cannot
// constrain it) and its two RoleBindings (gateway and controller SAs).
func (r *AgentChannelReconciler) ensureCredentialRole(ctx context.Context, channel *kaalmv1alpha1.AgentChannel) error {
	names := authSecretNames(channel)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: channelRoleName(channel.Name), Namespace: channel.Namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: names,
			Verbs:         []string{"get", "watch"},
		}},
	}
	if err := controllerutil.SetControllerReference(channel, role, r.Scheme()); err != nil {
		return err
	}
	var current rbacv1.Role
	key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
	if err := r.Get(ctx, key, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, role); err != nil {
			return err
		}
	} else if len(current.Rules) != 1 || !equalStrings(current.Rules[0].ResourceNames, names) {
		// Secret refs changed: shrink or grow the grant so no stale access
		// is retained.
		current.Rules = role.Rules
		if err := r.Update(ctx, &current); err != nil {
			return err
		}
	}

	for _, sa := range []string{"kaalm-gateway", "kaalm-controller"} {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: channelRoleName(channel.Name) + "-" + strings.TrimPrefix(sa, "kaalm-"), Namespace: channel.Namespace,
			},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: channelRoleName(channel.Name)},
			Subjects: []rbacv1.Subject{{
				Kind: "ServiceAccount", Name: sa, Namespace: r.OperatorNamespace,
			}},
		}
		if err := controllerutil.SetControllerReference(channel, rb, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// validateSecrets confirms every referenced Secret and key exists. The shared
// CredentialsMissing reason is the one stable "channel auth Secret unusable"
// signal for both directions.
func (r *AgentChannelReconciler) validateSecrets(ctx context.Context, channel *kaalmv1alpha1.AgentChannel) (string, string) {
	check := func(ref *kaalmv1alpha1.SecretKeyReference) (string, string) {
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: channel.Namespace, Name: ref.Name}, &sec); err != nil {
			return kaalmv1alpha1.ReasonCredentialsMissing,
				fmt.Sprintf("Secret %q not found in namespace %q", ref.Name, channel.Namespace)
		}
		if v, ok := sec.Data[ref.Key]; !ok || len(v) == 0 {
			return kaalmv1alpha1.ReasonCredentialsMissing,
				fmt.Sprintf("key %q missing in Secret %q", ref.Key, ref.Name)
		}
		return "", ""
	}
	auths := []*kaalmv1alpha1.ChannelAuth{&channel.Spec.Webhook.Auth}
	if channel.Spec.Webhook.CallbackURL != nil && channel.Spec.Webhook.CallbackAuth != nil {
		auths = append(auths, channel.Spec.Webhook.CallbackAuth)
	}
	for _, auth := range auths {
		if auth.SecretRef != nil {
			if reason, msg := check(auth.SecretRef); reason != "" {
				return reason, msg
			}
		}
		if auth.HMAC != nil {
			if reason, msg := check(&auth.HMAC.SecretRef); reason != "" {
				return reason, msg
			}
		}
	}
	return "", ""
}

// validateCallbackURL is the reconcile-time half of rule 22. Loopback,
// link-local (which covers the 169.254.169.254 cloud-metadata endpoint), and
// the unspecified address are always rejected; private (RFC1918/ULA) targets
// are rejected unless allowPrivate is set, which opts into in-cluster or
// self-hosted receivers. This mirrors the gateway's pre-dial blockedCallbackIP.
func validateCallbackURL(raw string, allowPrivate bool) (string, string) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return kaalmv1alpha1.ReasonInvalidCallbackURL, "callbackUrl must be a valid https URL"
	}
	host := parsed.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		// Unresolvable now is not a hard failure; the gateway re-checks
		// before every dial.
		return "", ""
	}
	for _, ip := range ips {
		blocked := ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
		if !allowPrivate && ip.IsPrivate() {
			blocked = true
		}
		if blocked {
			return kaalmv1alpha1.ReasonInvalidCallbackURL,
				fmt.Sprintf("callbackUrl host resolves to blocked address %s", ip)
		}
	}
	return "", ""
}

// reduceChannelHealth applies the 4-rule reduction into PlatformConnected.
func (r *AgentChannelReconciler) reduceChannelHealth(ctx context.Context, channel *kaalmv1alpha1.AgentChannel) {
	reachable, total, err := r.Health.NamespaceChannelHealth(ctx, channel.Namespace)
	if err != nil || total == 0 || len(reachable) == 0 {
		return // rule 4: preserve the existing condition
	}
	path := channel.Spec.Webhook.Path
	var lastSuccess, lastFailure *ChannelHealthState
	fullWindow := false
	allEmpty := true
	for i := range reachable {
		replica := &reachable[i]
		window := time.Duration(replica.WindowSeconds) * time.Second
		if window > 0 && time.Since(replica.StartedAt) >= window {
			fullWindow = true
		}
		state, ok := replica.Channels[path]
		if !ok || state.State == healthStateEmpty {
			continue
		}
		allEmpty = false
		switch state.State {
		case healthStateSuccess:
			if lastSuccess == nil || newerHealth(state, *lastSuccess) {
				s := state
				lastSuccess = &s
			}
		case healthStateFailure:
			if lastFailure == nil || newerHealth(state, *lastFailure) {
				s := state
				lastFailure = &s
			}
		}
	}

	cond := metav1.Condition{Type: kaalmv1alpha1.ConditionPlatformConnected}
	switch {
	case lastSuccess != nil: // rule 1
		cond.Status = metav1.ConditionTrue
		cond.Reason = kaalmv1alpha1.ReasonWebhookReady
		cond.Message = "webhook delivery succeeded within the health window"
	case lastFailure != nil: // rule 2
		cond.Status = metav1.ConditionFalse
		cond.Reason = deref(lastFailure.Reason, "DispatchFailed")
		cond.Message = deref(lastFailure.LastError, "delivery failed")
	case fullWindow && allEmpty: // rule 3
		cond.Status = metav1.ConditionUnknown
		cond.Reason = kaalmv1alpha1.ReasonNoRecentTraffic
		cond.Message = "no webhook traffic observed within the health window"
	default: // rule 4
		return
	}
	apimeta.SetStatusCondition(&channel.Status.Conditions, cond)
}

func newerHealth(a, b ChannelHealthState) bool {
	if a.Timestamp == nil || b.Timestamp == nil {
		return b.Timestamp == nil
	}
	return *a.Timestamp > *b.Timestamp
}

func deref(s *string, fallback string) string {
	if s != nil && *s != "" {
		return *s
	}
	return fallback
}

// reducePhase maps the referenced Agent's phase onto the Channel phase.
// status.phase and Ready are deliberately separate axes.
func (r *AgentChannelReconciler) reducePhase(channel *kaalmv1alpha1.AgentChannel, agent *kaalmv1alpha1.Agent) {
	switch agent.Status.Phase {
	case kaalmv1alpha1.AgentFailed, kaalmv1alpha1.AgentDegraded:
		channel.Status.Phase = kaalmv1alpha1.ChannelDegraded
	default:
		channel.Status.Phase = kaalmv1alpha1.ChannelActive
	}
}

// pruneAsyncConfigMaps deletes this channel's async response records: only
// expired ones on normal passes, all of them on the finalizer sweep.
func (r *AgentChannelReconciler) pruneAsyncConfigMaps(
	ctx context.Context, channel *kaalmv1alpha1.AgentChannel, sweepAll bool,
) error {
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.InNamespace(r.OperatorNamespace), client.MatchingLabels(map[string]string{
		kaalmv1alpha1.LabelChannelNamespace: channel.Namespace,
		kaalmv1alpha1.LabelChannelName:      channel.Name,
	})); err != nil {
		return err
	}
	now := time.Now()
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !strings.HasPrefix(cm.Name, "kaalm-async-") {
			continue
		}
		if !sweepAll {
			expiresAt, err := time.Parse(time.RFC3339, cm.Annotations[kaalmv1alpha1.AnnotationExpiresAt])
			if err != nil || expiresAt.After(now) {
				continue
			}
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileDelete drives the six-step delete handshake: announce Terminating,
// wait for the gateway's disconnect confirmation (bounded), sweep the async
// records once, release the finalizer.
func (r *AgentChannelReconciler) reconcileDelete(ctx context.Context, channel *kaalmv1alpha1.AgentChannel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(channel, kaalmv1alpha1.ChannelFinalizer) {
		return ctrl.Result{}, nil
	}

	// Step 1: announce. Gateway replicas observe this through their watch
	// and stop creating async records (the write gate).
	if channel.Status.Phase != kaalmv1alpha1.ChannelTerminating {
		channel.Status.Phase = kaalmv1alpha1.ChannelTerminating
		if err := r.Status().Update(ctx, channel); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Step 4: wait for the disconnect annotation, bounded so a dead gateway
	// cannot wedge deletion forever.
	disconnected := channel.Annotations[kaalmv1alpha1.AnnotationChannelDisconnected] == kaalmv1alpha1.AnnotationTrue
	if !disconnected && time.Since(channel.DeletionTimestamp.Time) < disconnectTimeout {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Step 5: the one-shot sweep. The write gate plus the confirmed (or
	// timed-out) disconnect is what makes a single sweep final.
	if err := r.pruneAsyncConfigMaps(ctx, channel, true); err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: release.
	controllerutil.RemoveFinalizer(channel, kaalmv1alpha1.ChannelFinalizer)
	return ctrl.Result{}, r.Update(ctx, channel)
}

func (r *AgentChannelReconciler) setChannelReady(channel *kaalmv1alpha1.AgentChannel, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&channel.Status.Conditions, metav1.Condition{
		Type: kaalmv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

// SetupWithManager wires the reconciler, its owned RBAC pair, and the Agent
// watch (phase reduction must track Agent phase changes).
func (r *AgentChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1alpha1.AgentChannel{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&kaalmv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.channelsForAgent)).
		Complete(r)
}

// channelsForAgent re-enqueues every channel referencing a changed Agent.
func (r *AgentChannelReconciler) channelsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	var channels kaalmv1alpha1.AgentChannelList
	if err := r.List(ctx, &channels, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ch := range channels.Items {
		if ch.Spec.AgentRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name}})
		}
	}
	return reqs
}
