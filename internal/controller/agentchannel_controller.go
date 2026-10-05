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
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/callbackpolicy"
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

// SecretChangeSource reports changes to the Secrets it watches. A
// secretwatch.Watcher satisfies it.
type SecretChangeSource interface {
	Subscribe(fn func(types.NamespacedName))
}

// AgentChannelReconciler validates channels, scopes credential access, reports
// status, and coordinates the delete handshake. It owns no Pods. See
// docs/src/controller/reconcilers/agentchannel.md.
type AgentChannelReconciler struct {
	client.Client
	Recorder          record.EventRecorder
	OperatorNamespace string
	// SecretReader reads Secrets in user namespaces, which the manager's
	// cache does not hold. In production it is a secretwatch.Reader: one
	// name-filtered watch per referenced Secret, so repeated reads are cache
	// hits. nil falls back to the embedded client. SecretChanges reports
	// changes to the Secrets read through it.
	SecretReader client.Reader
	// SecretChanges is the watcher behind SecretReader. When set, a change
	// to a Secret a channel references re-enqueues the channel at once; nil
	// leaves only the periodic pass.
	SecretChanges SecretChangeSource
	// MaxConcurrentReconciles is the number of reconciles that may run at
	// once; controller-runtime still serializes per object. 0 means one.
	MaxConcurrentReconciles int
	// Health polls per-channel gateway delivery health; nil preserves the
	// existing PlatformConnected condition.
	Health ChannelHealthClient
	// CallbackPolicy decides which callbackUrl targets rule 22 permits. The
	// zero value denies internal address space; entries come from
	// gateway.callbackUrl.allowlist and must match the gateway's, since the
	// gateway repeats the check pre-dial.
	CallbackPolicy callbackpolicy.Policy
	// LookupIP resolves the callbackUrl host for rule 22. nil means
	// net.DefaultResolver. Tests inject a fake.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)

	// unresolvedMu guards unresolvedHosts: per channel UID, the callback host
	// the last CallbackHostUnresolved Warning named. The Warning fires only
	// when this changes, so a channel that re-validates every minute raises
	// one event, not one per pass, and makes no event write per pass.
	unresolvedMu    sync.Mutex
	unresolvedHosts map[types.UID]string
}

// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentchannels/finalizers,verbs=update

// Reconcile runs one pass over an AgentChannel.
func (r *AgentChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var channel kaalmv1beta1.AgentChannel
	if err := r.Get(ctx, req.NamespacedName, &channel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !channel.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &channel)
	}
	if controllerutil.AddFinalizer(&channel, kaalmv1beta1.ChannelFinalizer) {
		if err := r.Update(ctx, &channel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	statusBefore := channel.Status.DeepCopy()
	channel.Status.ObservedGeneration = channel.Generation

	// The system-namespace guard runs first, as on the workload reconcilers.
	if channel.Namespace == r.OperatorNamespace {
		channel.Status.Phase = kaalmv1beta1.ChannelFailed
		if err := r.gateChannel(ctx, &channel, statusBefore, kaalmv1beta1.ReasonSystemNamespaceForbidden,
			fmt.Sprintf("AgentChannels may not live in the operator namespace %q", r.OperatorNamespace)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// The credential Roles follow the Secret labels on every pass, whatever
	// the checks below find; their result is reported in its own slot.
	credReason, credMsg := r.scopeCredentialRoles(ctx, &channel)

	// Step 1: resolve agentRef (an Agent, never an AgentTask).
	var agent kaalmv1beta1.Agent
	agentErr := r.Get(ctx, types.NamespacedName{Namespace: channel.Namespace, Name: channel.Spec.AgentRef.Name}, &agent)
	if agentErr != nil {
		if !apierrors.IsNotFound(agentErr) {
			return ctrl.Result{}, agentErr
		}
		channel.Status.Phase = kaalmv1beta1.ChannelFailed
		if err := r.gateChannel(ctx, &channel, statusBefore, kaalmv1beta1.ReasonAgentNotFound,
			fmt.Sprintf("Agent %q not found in namespace %q", channel.Spec.AgentRef.Name, channel.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		// The Agent watch fires once when the Agent is deleted and never
		// again, so this re-check is what prunes records that expire later.
		// Creating the Agent still re-runs the channel at once through
		// channelsForAgent.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Steps 2 and 3 validation chain; the first failure reports and stops.
	if reason, msg := r.validateChannel(ctx, &channel, &agent, credReason, credMsg); reason != "" {
		// A failing channel is not Ready, so the unresolved-host Warning has
		// nothing to add; clear it so it fires afresh once the channel passes.
		r.forgetCallbackResolution(&channel)
		r.reducePhase(&channel, &agent)
		// Re-check on the same cadence as a healthy channel. A change to a
		// referenced Secret re-enqueues the channel at once through
		// SecretChanges; this requeue is the fallback for a change no watch
		// reported (a Secret whose watch has not started or synced yet) and
		// for checks no watch covers, and it keeps the channel's Secret
		// watches in use so the watcher's idle janitor (one hour) never
		// stops them. A child conflict re-checks sooner, on the workloads'
		// cadence: the conflicting object carries no owner reference, so its
		// removal raises no watch event. This re-check also runs the expiry
		// prune, so an expired record of a failing channel goes within one
		// interval.
		requeue := time.Minute
		if reason == kaalmv1beta1.ReasonChildConflict {
			requeue = gateRequeue
		}
		if err := r.gateChannel(ctx, &channel, statusBefore, reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	r.setChannelReady(&channel, true, kaalmv1beta1.ReasonAgentReachable, "channel is valid")

	// Step 4: channel health poll and the tri-state reduction.
	if r.Health != nil {
		r.reduceChannelHealth(ctx, &channel)
	}

	// Step 5: phase reduction from the Agent's phase.
	r.reducePhase(&channel, &agent)

	// Step 6: write the status, then prune expired async response
	// ConfigMaps for this channel. The status goes first, as in gateChannel,
	// so a prune error never hides this pass's status.
	if err := r.updateStatusIfChanged(ctx, &channel, statusBefore); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneAsyncConfigMaps(ctx, &channel, false); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled AgentChannel", "phase", channel.Status.Phase)
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// scopeCredentialRoles runs step 3: the per-channel check Role, the one
// read of every referenced Secret, and the gateway's credential Role. It
// runs on every pass outside the operator namespace, before the Agent,
// service, and path checks, because the credential Role must follow the
// Secret labels whatever those checks find: a channel that is not Ready
// never keeps a grant wider than its labeled Secrets. None of its inputs
// depends on those checks. Once the Roles have converged, a pass costs only
// cache reads. It returns the first failure as a reason and message, which
// validateChannel reports after the earlier checks.
func (r *AgentChannelReconciler) scopeCredentialRoles(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel,
) (reason, msg string) {
	// Step 3: the controller-only check Role must exist BEFORE any Secret
	// read: the operator has no standing Secret read in user namespaces, and
	// RBAC has no label-scoped grant, so it needs get and watch on every
	// referenced name to see the rule 45 label. The gateway's Role follows
	// the reads and lists only the Secrets that opted in.
	names := authSecretNames(channel)
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	if err := ensureControllerSecretAccess(ctx, r.Client, r.Scheme(), channel, channelCheckRoleName(channel.Name),
		r.OperatorNamespace, refs); err != nil {
		reason, msg = kaalmv1beta1.ReasonInvalidReference, "ensuring the credential check Role failed: "+err.Error()
		if _, ok := asChildConflict(err); ok {
			reason, msg = kaalmv1beta1.ReasonChildConflict, err.Error()
		}
		// Without the check Role no label can be read, so no Secret counts
		// as opted in (readChannelSecrets follows the same rule for a failed
		// read): the gateway's Role is emptied. The Secrets are not read,
		// since every read would be Forbidden and retried. The next pass
		// retries both writes.
		if err := r.ensureCredentialRole(ctx, channel, nil); err != nil {
			log.FromContext(ctx).V(1).Info("emptying the channel credential Role failed", "error", err)
		}
		return reason, msg
	}
	reason, msg, optedIn := r.validateSecrets(ctx, channel)
	if err := r.ensureCredentialRole(ctx, channel, optedIn); err != nil {
		if _, ok := asChildConflict(err); ok {
			return kaalmv1beta1.ReasonChildConflict, err.Error()
		}
		return kaalmv1beta1.ReasonInvalidReference, "ensuring the credential Role failed: " + err.Error()
	}
	return reason, msg
}

// validateChannel runs steps 2 and 3: service enabled, path shape, path
// conflict, then the result of scopeCredentialRoles (the Role writes and
// Secret validation, rules 25, 40, 45, and 46), passed in as credReason and
// credMsg, and rule 22. Returns a non-empty reason on the first failure.
func (r *AgentChannelReconciler) validateChannel(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, agent *kaalmv1beta1.Agent, credReason, credMsg string,
) (string, string) {
	// Step 2: the Agent must expose a Service (delivery target).
	if agent.Spec.Service != nil && !agent.Spec.Service.Enabled {
		return kaalmv1beta1.ReasonAgentServiceDisabled,
			fmt.Sprintf("Agent %q has service.enabled=false; channels need a delivery target", agent.Name)
	}
	// Rule 15: the path must begin with /channels/{namespace}/. CRD CEL
	// cannot read metadata.namespace, so this lives here. The path comes from
	// whichever block the type selects (webhook, discord, whatsapp); rule 39
	// (CRD CEL) guarantees exactly one is set.
	path := channel.Spec.Path()
	prefix := "/channels/" + channel.Namespace + "/"
	if path == "" {
		return kaalmv1beta1.ReasonInvalidPath,
			fmt.Sprintf("spec.%s is not set for type %q", channelType(channel), channelType(channel))
	}
	if !strings.HasPrefix(path, prefix) {
		return kaalmv1beta1.ReasonInvalidPath,
			fmt.Sprintf("%s.path must begin with %q", channelType(channel), prefix)
	}
	// Path conflict: the earliest creationTimestamp wins, across every type.
	// A change to any channel on this path re-runs this check for the others
	// (pathSiblingHandler), so the loser's status follows at once.
	if channels, err := r.channelsOnPath(ctx, channel.Namespace, path); err == nil {
		for i := range channels {
			other := &channels[i]
			if other.Name == channel.Name {
				continue
			}
			if other.CreationTimestamp.Before(&channel.CreationTimestamp) ||
				(other.CreationTimestamp.Equal(&channel.CreationTimestamp) && other.Name < channel.Name) {
				return kaalmv1beta1.ReasonPathConflict,
					fmt.Sprintf("path %q is already registered by the older channel %q", path, other.Name)
			}
		}
	}
	if credReason != "" {
		return credReason, credMsg
	}
	// Rule 22: callbackUrl must be HTTPS and must not point into internal
	// address space (reconcile-time half; the gateway re-checks pre-dial).
	// Webhook channels only: a platform channel replies through the
	// operator-set platform API base URL and has no callbackUrl.
	unresolved := ""
	if channel.Spec.Webhook != nil && channel.Spec.Webhook.CallbackURL != nil {
		reason, msg, host := validateCallbackURL(ctx, *channel.Spec.Webhook.CallbackURL, r.CallbackPolicy, r.lookupIP)
		if reason != "" {
			return reason, msg
		}
		unresolved = host
	}
	r.noteCallbackResolution(channel, unresolved)
	return "", ""
}

// lookupIP resolves a callback host through the injected resolver, or the
// default one.
func (r *AgentChannelReconciler) lookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if r.LookupIP != nil {
		return r.LookupIP(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// noteCallbackResolution records whether the channel's callback host resolved
// on this pass (host is empty when it did, or when there is no callbackUrl)
// and emits the CallbackHostUnresolved Warning when the unresolved host
// changes. The channel stays Ready: a DNS blip must not take a working channel
// down, and the gateway re-checks the host before every dial.
func (r *AgentChannelReconciler) noteCallbackResolution(channel *kaalmv1beta1.AgentChannel, host string) {
	r.unresolvedMu.Lock()
	last, seen := r.unresolvedHosts[channel.UID]
	switch {
	case host == "":
		delete(r.unresolvedHosts, channel.UID)
	case !seen || last != host:
		if r.unresolvedHosts == nil {
			r.unresolvedHosts = map[types.UID]string{}
		}
		r.unresolvedHosts[channel.UID] = host
	}
	r.unresolvedMu.Unlock()
	if host == "" || (seen && last == host) || r.Recorder == nil {
		return
	}
	r.Recorder.Event(channel, corev1.EventTypeWarning, kaalmv1beta1.ReasonCallbackHostUnresolved,
		fmt.Sprintf("callbackUrl host %q does not resolve; the channel stays Ready and the gateway "+
			"checks the host again before every callback delivery", host))
}

// forgetCallbackResolution drops a deleted channel's entry.
func (r *AgentChannelReconciler) forgetCallbackResolution(channel *kaalmv1beta1.AgentChannel) {
	r.unresolvedMu.Lock()
	delete(r.unresolvedHosts, channel.UID)
	r.unresolvedMu.Unlock()
}

// channelType names the block the spec's type selects, for messages.
func channelType(channel *kaalmv1beta1.AgentChannel) string {
	if channel.Spec.Type == "" {
		return kaalmv1beta1.ChannelTypeWebhook
	}
	return channel.Spec.Type
}

// authSecretNames collects the Secret names the channel's credentials
// reference: for a webhook channel the inbound auth Secret always and the
// callbackAuth Secret when callbackUrl is set; for a platform channel the
// single credentialsRef Secret (rule 40).
func authSecretNames(channel *kaalmv1beta1.AgentChannel) []string {
	set := map[string]bool{}
	collect := func(auth *kaalmv1beta1.ChannelAuth) {
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
	switch {
	case channel.Spec.Discord != nil && channelType(channel) == kaalmv1beta1.ChannelTypeDiscord:
		set[channel.Spec.Discord.CredentialsRef.Name] = true
	case channel.Spec.WhatsApp != nil && channelType(channel) == kaalmv1beta1.ChannelTypeWhatsApp:
		set[channel.Spec.WhatsApp.CredentialsRef.Name] = true
	case channel.Spec.Webhook != nil:
		collect(&channel.Spec.Webhook.Auth)
		if channel.Spec.Webhook.CallbackURL != nil {
			collect(channel.Spec.Webhook.CallbackAuth)
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func channelRoleName(channelName string) string { return "kaalm-channel-" + channelName + "-creds" }

// channelControllerCredsBindingName names the RoleBinding that once bound the
// operator to the credential Role; the reconciler removes it when the channel
// controls it.
func channelControllerCredsBindingName(channelName string) string {
	return channelRoleName(channelName) + "-controller"
}

// channelCheckRoleName names the controller-only Role that lets the reconciler
// read every Secret the channel references, to check the rule 45 label.
func channelCheckRoleName(channelName string) string {
	return "kaalm-channel-" + channelName + "-check"
}

// ensureCredentialRole creates or updates the per-channel gateway-facing Role
// (get, watch, resourceNames-scoped; list deliberately omitted since
// resourceNames cannot constrain it) and its single RoleBinding to the gateway
// ServiceAccount, then removes the controller's binding to that Role when the
// channel controls it. The Role lists only names, the referenced Secrets that
// carry the rule 45 label; with none it has no rules at all, never one rule
// with empty resourceNames, which would grant every Secret.
func (r *AgentChannelReconciler) ensureCredentialRole(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, names []string,
) error {
	var rules []rbacv1.PolicyRule
	if len(names) > 0 {
		rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: names,
			Verbs:         []string{"get", "watch"},
		}}
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: channelRoleName(channel.Name), Namespace: channel.Namespace},
		Rules:      rules,
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
		if err := createControlled(ctx, r.Client, channel, role); err != nil {
			return err
		}
	} else if err := requireControlled(r.Scheme(), channel, &current); err != nil {
		return err
	} else if !equality.Semantic.DeepEqual(current.Rules, rules) {
		// Secret refs or labels changed: shrink or grow the grant so no
		// stale access is retained.
		current.Rules = rules
		if err := r.Update(ctx, &current); err != nil {
			return err
		}
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: channelRoleName(channel.Name) + "-gateway", Namespace: channel.Namespace,
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: channelRoleName(channel.Name)},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: gatewayServiceAccount, Namespace: r.OperatorNamespace,
		}},
	}
	if err := controllerutil.SetControllerReference(channel, rb, r.Scheme()); err != nil {
		return err
	}
	// Read from the informer before writing: a create that is expected to
	// fail AlreadyExists is still a POST the apiserver has to reject, and the
	// reconciler runs every minute for every channel (#174).
	var currentRB rbacv1.RoleBinding
	err := r.Get(ctx, types.NamespacedName{Namespace: rb.Namespace, Name: rb.Name}, &currentRB)
	switch {
	case apierrors.IsNotFound(err):
		if err := createControlled(ctx, r.Client, channel, rb); err != nil {
			return err
		}
	case err != nil:
		return err
	case requireControlled(r.Scheme(), channel, &currentRB) != nil:
		return requireControlled(r.Scheme(), channel, &currentRB)
	case currentRB.RoleRef != rb.RoleRef || !equality.Semantic.DeepEqual(currentRB.Subjects, rb.Subjects):
		currentRB.RoleRef = rb.RoleRef
		currentRB.Subjects = rb.Subjects
		if err := r.Update(ctx, &currentRB); err != nil {
			return err
		}
	}
	return r.removeControllerCredsBinding(ctx, channel)
}

// removeControllerCredsBinding deletes the RoleBinding that bound the
// operator's ServiceAccount to the credential Role. The operator needs no
// binding to that Role: the check Role already grants it every Secret name
// the credential Role lists. A RoleBinding of this name that the channel does
// not control is not the channel's and is left alone. A missing one costs a
// single cached read and no write.
func (r *AgentChannelReconciler) removeControllerCredsBinding(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel,
) error {
	var rb rbacv1.RoleBinding
	key := types.NamespacedName{Namespace: channel.Namespace, Name: channelControllerCredsBindingName(channel.Name)}
	if err := r.Get(ctx, key, &rb); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&rb, channel) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &rb))
}

// updateStatusIfChanged writes the channel's status only when a pass changed
// it. The reconciler requeues every channel every minute, and a status write
// per pass was the largest single write the controller made under load,
// with nothing in it new (#174).
func (r *AgentChannelReconciler) updateStatusIfChanged(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, before *kaalmv1beta1.AgentChannelStatus,
) error {
	if equality.Semantic.DeepEqual(before, &channel.Status) {
		return nil
	}
	return r.Status().Update(ctx, channel)
}

// channelSecretRead is one referenced Secret as this pass read it.
type channelSecretRead struct {
	sec *corev1.Secret
	err error
}

// readChannelSecrets reads every Secret the channel references exactly once
// and returns the reads by name, plus the sorted names that carry the rule 45
// label. A read that failed, for any reason, is not opted in.
func (r *AgentChannelReconciler) readChannelSecrets(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel,
) (map[string]channelSecretRead, []string) {
	reads := map[string]channelSecretRead{}
	var optedIn []string
	reader := liveSecretReader(r.SecretReader, r.Client)
	for _, name := range authSecretNames(channel) {
		var sec corev1.Secret
		err := getSecretLive(ctx, reader, types.NamespacedName{Namespace: channel.Namespace, Name: name}, &sec)
		if err != nil {
			reads[name] = channelSecretRead{err: err}
			continue
		}
		reads[name] = channelSecretRead{sec: &sec}
		if kaalmv1beta1.ChannelCredentialOptedIn(sec.Labels) {
			optedIn = append(optedIn, name)
		}
	}
	return reads, optedIn // authSecretNames is sorted, so optedIn is too
}

// notOptedInMessage words a rule 45 failure. It names the Secret and the label
// only: nothing about the keys of a Secret that did not opt in.
func notOptedInMessage(name string) string {
	return fmt.Sprintf("Secret %q does not carry the label %s: %q; a channel may use only Secrets with that label",
		name, kaalmv1beta1.LabelChannelCredential, kaalmv1beta1.AnnotationTrue)
}

// validateSecrets reads every referenced Secret once and checks each
// reference in order: the Secret is readable, it carries the rule 45 label,
// a bearer callbackAuth Secret approves the callbackUrl host (rule 46), and
// then the key checks. The inbound auth Secret reports the shared
// CredentialsMissing reason; the outbound callbackAuth Secret (rule 25)
// reports CallbackAuthMissing when the Secret or key is absent and
// CallbackAuthInvalid when the key is empty or the block names no Secret for
// its type. It also returns the names that carry the label, for the
// gateway's Role.
func (r *AgentChannelReconciler) validateSecrets(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel,
) (reason, msg string, optedIn []string) {
	reads, optedIn := r.readChannelSecrets(ctx, channel)
	reason, msg = checkChannelSecrets(channel, reads)
	return reason, msg, optedIn
}

// readOf returns the pass's read of one Secret. Every name authSecretNames
// returns was read; any other name reads as not found.
func readOf(reads map[string]channelSecretRead, name string) channelSecretRead {
	if read, ok := reads[name]; ok {
		return read
	}
	return channelSecretRead{err: apierrors.NewNotFound(corev1.Resource("secrets"), name)}
}

// checkChannelSecrets runs the ordered checks of validateSecrets over the
// pass's reads.
func checkChannelSecrets(channel *kaalmv1beta1.AgentChannel, reads map[string]channelSecretRead) (string, string) {
	// check runs one reference. host is the callbackUrl host a bearer
	// callback token must be approved for, or "" when rule 46 does not apply.
	check := func(ref *kaalmv1beta1.SecretKeyReference, missing, empty, host string) (string, string) {
		read := readOf(reads, ref.Name)
		if read.err != nil {
			return missing, secretReadMessage(read.err, ref.Name, channel.Namespace)
		}
		if !kaalmv1beta1.ChannelCredentialOptedIn(read.sec.Labels) {
			return kaalmv1beta1.ReasonSecretNotOptedIn, notOptedInMessage(ref.Name)
		}
		if host != "" && !kaalmv1beta1.CallbackHostApproved(read.sec.Annotations, host) {
			return kaalmv1beta1.ReasonCallbackHostNotApproved,
				fmt.Sprintf("Secret %q does not list the callbackUrl host %q in its %s annotation",
					ref.Name, host, kaalmv1beta1.AnnotationCallbackHosts)
		}
		v, ok := read.sec.Data[ref.Key]
		if !ok {
			return missing, fmt.Sprintf("key %q missing in Secret %q", ref.Key, ref.Name)
		}
		if len(v) == 0 {
			return empty, fmt.Sprintf("key %q is empty in Secret %q", ref.Key, ref.Name)
		}
		return "", ""
	}
	switch {
	case channel.Spec.Discord != nil && channelType(channel) == kaalmv1beta1.ChannelTypeDiscord:
		name := channel.Spec.Discord.CredentialsRef.Name
		return validatePlatformSecret(readOf(reads, name), channel.Namespace, name,
			[]string{discordKeyPublicKey}, validateDiscordPublicKey)
	case channel.Spec.WhatsApp != nil && channelType(channel) == kaalmv1beta1.ChannelTypeWhatsApp:
		name := channel.Spec.WhatsApp.CredentialsRef.Name
		return validatePlatformSecret(readOf(reads, name), channel.Namespace, name,
			[]string{whatsAppKeyVerifyToken, whatsAppKeyAppSecret, whatsAppKeyAccessToken}, nil)
	case channel.Spec.Webhook == nil:
		return "", ""
	}
	inbound := &channel.Spec.Webhook.Auth
	for _, ref := range authSecretRefs(inbound) {
		if reason, msg := check(ref, kaalmv1beta1.ReasonCredentialsMissing, kaalmv1beta1.ReasonCredentialsMissing, ""); reason != "" {
			return reason, msg
		}
	}
	callback := channel.Spec.Webhook.CallbackAuth
	if channel.Spec.Webhook.CallbackURL == nil || callback == nil {
		return "", ""
	}
	// CRD CEL requires the ref the type names; the reconciler repeats the
	// check so a block that slips past it reads as malformed, not as valid.
	if (callback.Type == authTypeBearer && callback.SecretRef == nil) || (callback.Type == "hmac" && callback.HMAC == nil) {
		return kaalmv1beta1.ReasonCallbackAuthInvalid,
			fmt.Sprintf("callbackAuth type %q names no Secret", callback.Type)
	}
	// Rule 46 binds a bearer callback token to the hosts its Secret approves.
	// An HMAC callback sends a signature, never the key, so it is exempt. A
	// callbackUrl that is not a valid https URL is left to rule 22.
	host := ""
	if callback.Type == authTypeBearer {
		host = callbackHost(*channel.Spec.Webhook.CallbackURL)
	}
	for _, ref := range authSecretRefs(callback) {
		if reason, msg := check(ref, kaalmv1beta1.ReasonCallbackAuthMissing, kaalmv1beta1.ReasonCallbackAuthInvalid, host); reason != "" {
			return reason, "callbackAuth: " + msg
		}
	}
	return "", ""
}

// authTypeBearer is the ChannelAuth type that sends the Secret value itself.
const authTypeBearer = "bearer"

// callbackHost returns the hostname of an https callbackUrl, without its
// port, or "" when raw is not a valid https URL (rule 22 reports that).
func callbackHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != schemeHTTPS {
		return ""
	}
	return parsed.Hostname()
}

// authSecretRefs lists the Secret references one auth block carries.
func authSecretRefs(auth *kaalmv1beta1.ChannelAuth) []*kaalmv1beta1.SecretKeyReference {
	var refs []*kaalmv1beta1.SecretKeyReference
	if auth.SecretRef != nil {
		refs = append(refs, auth.SecretRef)
	}
	if auth.HMAC != nil {
		refs = append(refs, &auth.HMAC.SecretRef)
	}
	return refs
}

// The credential Secret keys the platform adapters read (rule 40). The
// gateway reads the same keys; the names are the contract in
// docs/src/resources/agentchannel.md, Platform types.
const (
	discordKeyPublicKey    = "publicKey"
	discordKeyBotToken     = "botToken"
	whatsAppKeyVerifyToken = "verifyToken"
	whatsAppKeyAppSecret   = "appSecret"
	whatsAppKeyAccessToken = "accessToken"
)

// secretReadMessage words a failed credential read. Only a NotFound answer
// says the Secret is absent; anything else (the scoped Role not yet honored,
// an apiserver error) is reported as it is.
func secretReadMessage(err error, name, namespace string) string {
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("Secret %q not found in namespace %q", name, namespace)
	}
	return fmt.Sprintf("Secret %q in namespace %q is not readable: %v", name, namespace, err)
}

// validatePlatformSecret is rules 45 and 40 for one platform channel: the
// Secret exists, carries the opt-in label, carries every required key, and
// (when a shape check is given) the key the adapter builds its verifier from
// is well-formed. A malformed key is CredentialsInvalid rather than
// CredentialsMissing, because the operator's fix is different: the value is
// there, it is wrong. A Secret without the label gets no key messages.
func validatePlatformSecret(
	read channelSecretRead, namespace, name string, required []string,
	shape func(data map[string][]byte) string,
) (string, string) {
	if read.err != nil {
		return kaalmv1beta1.ReasonCredentialsMissing, secretReadMessage(read.err, name, namespace)
	}
	if !kaalmv1beta1.ChannelCredentialOptedIn(read.sec.Labels) {
		return kaalmv1beta1.ReasonSecretNotOptedIn, notOptedInMessage(name)
	}
	sec := read.sec
	for _, key := range required {
		if v, ok := sec.Data[key]; !ok || len(v) == 0 {
			return kaalmv1beta1.ReasonCredentialsMissing,
				fmt.Sprintf("key %q missing in Secret %q", key, name)
		}
	}
	if shape != nil {
		if msg := shape(sec.Data); msg != "" {
			return kaalmv1beta1.ReasonCredentialsInvalid, fmt.Sprintf("Secret %q: %s", name, msg)
		}
	}
	return "", ""
}

// validateDiscordPublicKey checks that publicKey is an Ed25519 public key in
// hex (32 bytes), the only shape the adapter's verifier accepts.
func validateDiscordPublicKey(data map[string][]byte) string {
	raw := strings.TrimSpace(string(data[discordKeyPublicKey]))
	key, err := hex.DecodeString(raw)
	if err != nil {
		return "publicKey is not hex: " + err.Error()
	}
	if len(key) != ed25519.PublicKeySize {
		return fmt.Sprintf("publicKey decodes to %d bytes, want %d (an Ed25519 public key)", len(key), ed25519.PublicKeySize)
	}
	return ""
}

// validateCallbackURL is the reconcile-time half of rule 22. The target policy
// itself lives in internal/callbackpolicy, shared with the gateway's pre-dial
// re-check so the two halves cannot drift. A host that does not resolve passes
// the check and comes back as unresolved, for the caller to warn about.
func validateCallbackURL(
	ctx context.Context, raw string, policy callbackpolicy.Policy,
	lookup func(context.Context, string) ([]net.IP, error),
) (reason, msg, unresolved string) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Hostname() == "" {
		return kaalmv1beta1.ReasonInvalidCallbackURL, "callbackUrl must be a valid https URL", ""
	}
	host := parsed.Hostname()
	ips, err := lookup(ctx, host)
	if err != nil {
		return "", "", host
	}
	for _, ip := range ips {
		if !policy.Allowed(host, ip) {
			return kaalmv1beta1.ReasonInvalidCallbackURL,
				fmt.Sprintf("callbackUrl host resolves to blocked address %s", ip), ""
		}
	}
	return "", "", ""
}

// reduceChannelHealth applies the 4-rule reduction into PlatformConnected.
func (r *AgentChannelReconciler) reduceChannelHealth(ctx context.Context, channel *kaalmv1beta1.AgentChannel) {
	reachable, total, err := r.Health.NamespaceChannelHealth(ctx, channel.Namespace)
	if err != nil || total == 0 || len(reachable) == 0 {
		return // rule 4: preserve the existing condition
	}
	path := channel.Spec.Path()
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

	cond := metav1.Condition{Type: kaalmv1beta1.ConditionPlatformConnected}
	switch {
	case lastSuccess != nil: // rule 1
		cond.Status = metav1.ConditionTrue
		cond.Reason = kaalmv1beta1.ReasonWebhookReady
		cond.Message = "webhook delivery succeeded within the health window"
	case lastFailure != nil: // rule 2
		cond.Status = metav1.ConditionFalse
		cond.Reason = deref(lastFailure.Reason, "DispatchFailed")
		cond.Message = deref(lastFailure.LastError, "delivery failed")
	case fullWindow && allEmpty: // rule 3
		cond.Status = metav1.ConditionUnknown
		cond.Reason = kaalmv1beta1.ReasonNoRecentTraffic
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
func (r *AgentChannelReconciler) reducePhase(channel *kaalmv1beta1.AgentChannel, agent *kaalmv1beta1.Agent) {
	switch agent.Status.Phase {
	case kaalmv1beta1.AgentFailed, kaalmv1beta1.AgentDegraded:
		channel.Status.Phase = kaalmv1beta1.ChannelDegraded
	default:
		channel.Status.Phase = kaalmv1beta1.ChannelActive
	}
}

// pruneAsyncConfigMaps deletes this channel's async response records. A
// normal pass deletes the ones asyncRecordExpired reports, which includes the
// creationTimestamp fallback for a record with no parseable expiry; the
// finalizer sweep deletes them all.
func (r *AgentChannelReconciler) pruneAsyncConfigMaps(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, sweepAll bool,
) error {
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.InNamespace(r.OperatorNamespace), client.MatchingLabels(map[string]string{
		kaalmv1beta1.LabelChannelNamespace: channel.Namespace,
		kaalmv1beta1.LabelChannelName:      channel.Name,
	})); err != nil {
		return err
	}
	now := time.Now()
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !strings.HasPrefix(cm.Name, "kaalm-async-") {
			continue
		}
		if !sweepAll && !asyncRecordExpired(cm, now) {
			continue
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
func (r *AgentChannelReconciler) reconcileDelete(ctx context.Context, channel *kaalmv1beta1.AgentChannel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(channel, kaalmv1beta1.ChannelFinalizer) {
		return ctrl.Result{}, nil
	}

	// Step 1: announce. Gateway replicas observe this through their watch
	// and stop creating async records (the write gate).
	if channel.Status.Phase != kaalmv1beta1.ChannelTerminating {
		channel.Status.Phase = kaalmv1beta1.ChannelTerminating
		if err := r.Status().Update(ctx, channel); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Step 4: wait for the disconnect annotation, bounded so a dead gateway
	// cannot wedge deletion forever.
	disconnected := channel.Annotations[kaalmv1beta1.AnnotationChannelDisconnected] == kaalmv1beta1.AnnotationTrue
	if !disconnected && time.Since(channel.DeletionTimestamp.Time) < disconnectTimeout {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Step 5: the one-shot sweep. The write gate plus the confirmed (or
	// timed-out) disconnect is what makes a single sweep final.
	if err := r.pruneAsyncConfigMaps(ctx, channel, true); err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: release.
	r.forgetCallbackResolution(channel)
	controllerutil.RemoveFinalizer(channel, kaalmv1beta1.ChannelFinalizer)
	return ctrl.Result{}, r.Update(ctx, channel)
}

// gateChannel sets Ready=False for a validation failure, writes the status if
// the pass changed it, emits a Warning event with the same reason when the
// reason first appears (not on each pass that finds the problem again), then
// prunes the channel's expired async records. The event follows a successful
// write, so a pass that lost its write to a conflict does not report the
// reason twice. A channel that went invalid can still hold records it wrote
// while Ready, and the gateway writes none while it is not Ready, so this
// prune is what removes them. The status is written before the prune, so a
// prune error never hides the gate's status.
func (r *AgentChannelReconciler) gateChannel(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, before *kaalmv1beta1.AgentChannelStatus,
	reason, msg string,
) error {
	first := readyFalseIsNew(channel.Status.Conditions, reason)
	r.setChannelReady(channel, false, reason, msg)
	if err := r.updateStatusIfChanged(ctx, channel, before); err != nil {
		return err
	}
	if first && r.Recorder != nil {
		r.Recorder.Event(channel, corev1.EventTypeWarning, reason, msg)
	}
	return r.pruneAsyncConfigMaps(ctx, channel, false)
}

func (r *AgentChannelReconciler) setChannelReady(channel *kaalmv1beta1.AgentChannel, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&channel.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

// SetupWithManager wires the reconciler, its owned RBAC pair, the Agent
// watch (phase reduction must track Agent phase changes), the path sibling
// watch (a rule 15 conflict's outcome depends on every channel on the
// path), and, when SecretChanges is set, the referenced-Secret watch (the
// rule 25, 40, 45, and 46 checks read each Secret's label, annotation, and
// keys).
func (r *AgentChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&kaalmv1beta1.AgentChannel{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.channelsForAgent)).
		Watches(&kaalmv1beta1.AgentChannel{}, r.pathSiblingHandler())
	if r.SecretChanges != nil {
		// The controller cannot list user-namespace Secrets, so the watch
		// rides on the per-Secret informers the reads already started.
		b = b.WatchesRawSource(source.Func(func(
			ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request],
		) error {
			r.SecretChanges.Subscribe(func(key types.NamespacedName) {
				if ctx.Err() != nil {
					return
				}
				for _, req := range r.channelsForSecret(ctx, key) {
					q.Add(req)
				}
			})
			return nil
		}))
	}
	return b.Complete(r)
}

// channelsOnPath lists the channels in namespace registered at path, through
// the IndexChannelPath field index.
func (r *AgentChannelReconciler) channelsOnPath(
	ctx context.Context, namespace, path string,
) ([]kaalmv1beta1.AgentChannel, error) {
	var channels kaalmv1beta1.AgentChannelList
	if err := r.List(ctx, &channels, client.InNamespace(namespace),
		client.MatchingFields{IndexChannelPath: path}); err != nil {
		return nil, err
	}
	return channels.Items, nil
}

// pathSiblingHandler re-enqueues the other channels on a channel's path when
// the channel is created or deleted, and the other channels on both its old
// and its new path when its path changes. Which channel wins a rule 15 path
// conflict depends on every channel on the path, so without this a channel
// that lost to a newly created one (a timestamp tie lost by name, or a clock
// that went back), or that won once the older one left, would show its old
// Ready condition until its one-minute requeue. An update that keeps the
// path, such as a status write, enqueues nothing.
func (r *AgentChannelReconciler) pathSiblingHandler() handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueuePathSiblings(ctx, q, e.Object)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if channelPath(e.ObjectOld) == channelPath(e.ObjectNew) {
				return
			}
			r.enqueuePathSiblings(ctx, q, e.ObjectOld)
			r.enqueuePathSiblings(ctx, q, e.ObjectNew)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueuePathSiblings(ctx, q, e.Object)
		},
	}
}

// channelPath returns an AgentChannel's path, or "" for any other object.
func channelPath(obj client.Object) string {
	if ch, ok := obj.(*kaalmv1beta1.AgentChannel); ok {
		return ch.Spec.Path()
	}
	return ""
}

// enqueuePathSiblings adds every channel other than obj that shares obj's
// path in obj's namespace. A channel with no path has no siblings.
func (r *AgentChannelReconciler) enqueuePathSiblings(
	ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request], obj client.Object,
) {
	path := channelPath(obj)
	if path == "" {
		return
	}
	siblings, err := r.channelsOnPath(ctx, obj.GetNamespace(), path)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing channels on a path", "path", path)
		return
	}
	for i := range siblings {
		if siblings[i].Name == obj.GetName() {
			continue
		}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: siblings[i].Namespace, Name: siblings[i].Name}})
	}
}

// channelsForAgent re-enqueues every channel referencing a changed Agent,
// through the IndexChannelAgentRef field index.
func (r *AgentChannelReconciler) channelsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	var channels kaalmv1beta1.AgentChannelList
	if err := r.List(ctx, &channels, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{IndexChannelAgentRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(channels.Items))
	for _, ch := range channels.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name}})
	}
	return reqs
}

// channelsForSecret re-enqueues every channel whose credentials reference
// the changed Secret, through the IndexChannelSecretRef field index.
func (r *AgentChannelReconciler) channelsForSecret(ctx context.Context, key types.NamespacedName) []reconcile.Request {
	var channels kaalmv1beta1.AgentChannelList
	if err := r.List(ctx, &channels, client.InNamespace(key.Namespace),
		client.MatchingFields{IndexChannelSecretRef: key.Name}); err != nil {
		// The periodic pass still re-checks the channels.
		log.FromContext(ctx).Error(err, "listing channels referencing a Secret", "secret", key)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(channels.Items))
	for _, ch := range channels.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name}})
	}
	return reqs
}
