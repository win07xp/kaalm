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

package v1alpha1

// Phase enums. Phases are set by the controller and reported in status; they are
// not written by users, so they carry no CEL enum markers. See
// docs/src/controller/agent-lifecycle.md and task-lifecycle.md.

// AgentPhase is a point in the Agent lifecycle.
type AgentPhase string

const (
	AgentPending      AgentPhase = "Pending"
	AgentProvisioning AgentPhase = "Provisioning"
	AgentRunning      AgentPhase = "Running"
	AgentIdle         AgentPhase = "Idle"
	AgentHibernating  AgentPhase = "Hibernating"
	AgentHibernated   AgentPhase = "Hibernated"
	AgentResuming     AgentPhase = "Resuming"
	AgentDegraded     AgentPhase = "Degraded"
	AgentFailed       AgentPhase = "Failed"
	AgentTerminating  AgentPhase = "Terminating"
)

// AgentTaskPhase is a point in the AgentTask lifecycle. AgentTask has no Degraded
// phase: an irreconcilable task goes straight to Failed.
type AgentTaskPhase string

const (
	TaskPending      AgentTaskPhase = "Pending"
	TaskProvisioning AgentTaskPhase = "Provisioning"
	TaskRunning      AgentTaskPhase = "Running"
	TaskCompleting   AgentTaskPhase = "Completing"
	TaskSucceeded    AgentTaskPhase = "Succeeded"
	TaskFailed       AgentTaskPhase = "Failed"
	TaskTimedOut     AgentTaskPhase = "TimedOut"
	TaskTerminating  AgentTaskPhase = "Terminating"
)

// AgentChannelPhase is a point in the AgentChannel lifecycle. It is unset until
// the first reconcile.
type AgentChannelPhase string

const (
	ChannelActive      AgentChannelPhase = "Active"
	ChannelDegraded    AgentChannelPhase = "Degraded"
	ChannelFailed      AgentChannelPhase = "Failed"
	ChannelTerminating AgentChannelPhase = "Terminating"
)

// Condition types, keyed by resource. Ready is common to all. See the per-page
// condition inventories in docs/src/resources/ and controller/.
const (
	ConditionReady               = "Ready"
	ConditionFQDNPolicySupported = "FQDNPolicySupported" // AgentClass
	ConditionHealthy             = "Healthy"             // ModelProvider
	ConditionGatewayReachable    = "GatewayReachable"    // ModelProvider, Agent
	ConditionProvidersReady      = "ProvidersReady"      // Agent
	ConditionDegraded            = "Degraded"            // Agent: recoverable condition, distinct from the Degraded phase
	ConditionCompleted           = "Completed"           // AgentTask
	ConditionPlatformConnected   = "PlatformConnected"   // AgentChannel, tri-state
)

// Condition/event reason strings. Collected from the reconciler and validation
// pages; cited by number and name across the design.
const (
	ReasonAllReferencesResolved      = "AllReferencesResolved"
	ReasonInvalidCIDR                = "InvalidCIDR"
	ReasonFQDNPolicyUnsupported      = "FQDNPolicyUnsupported"
	ReasonCredentialsMissing         = "CredentialsMissing"
	ReasonCredentialsValid           = "CredentialsValid"
	ReasonCredentialsInvalid         = "CredentialsInvalid"
	ReasonUpstreamReachable          = "UpstreamReachable"
	ReasonInvalidDegradeTarget       = "InvalidDegradeTarget"
	ReasonDegradeTargetNotCheapest   = "DegradeTargetNotCheapest"
	ReasonFallbackIneligible         = "FallbackIneligible"
	ReasonSystemNamespaceForbidden   = "SystemNamespaceForbidden"
	ReasonClassConstraintViolation   = "ClassConstraintViolation"
	ReasonPersistenceNotAllowed      = "PersistenceNotAllowed"
	ReasonHibernationNotAllowed      = "HibernationNotAllowed"
	ReasonHibernationRequiresPersist = "HibernationRequiresPersistence"
	ReasonImagePullSecretMissing     = "ImagePullSecretMissing"
	ReasonExistingClaimNotFound      = "ExistingClaimNotFound"
	ReasonWakeIgnored                = "WakeIgnored"
	ReasonPodRunning                 = "PodRunning"
	ReasonAllProvidersHealthy        = "AllProvidersHealthy"
	ReasonAgentNotFound              = "AgentNotFound"
	ReasonAgentServiceDisabled       = "AgentServiceDisabled"
	ReasonInvalidPath                = "InvalidPath"
	ReasonPathConflict               = "PathConflict"
	ReasonInvalidCallbackURL         = "InvalidCallbackUrl"
	ReasonCallbackAuthMissing        = "CallbackAuthMissing"
	ReasonCallbackAuthInvalid        = "CallbackAuthInvalid"
	ReasonAgentReachable             = "AgentReachable"
	ReasonWebhookReady               = "WebhookReady"
	ReasonNoRecentTraffic            = "NoRecentTraffic"
	ReasonStalePodCompletion         = "StalePodCompletion"
	ReasonTaskAlreadyCompleted       = "TaskAlreadyCompleted"
	ReasonPhaseChanged               = "PhaseChanged"
	ReasonProviderUnhealthy          = "ProviderUnhealthy"
	ReasonBudgetExhausted            = "BudgetExhausted"
	ReasonInvalidReference           = "InvalidReference"
	ReasonHibernated                 = "Hibernated"
	ReasonWoken                      = "Woken"
	ReasonTaskSucceeded              = "TaskSucceeded"
	ReasonTaskFailed                 = "TaskFailed"
)

// Finalizers, one per CRD. See docs/src/controller/finalizers.md.
const (
	AgentFinalizer    = "agentry.io/agent-finalizer"
	TaskFinalizer     = "agentry.io/task-finalizer"
	ProviderFinalizer = "agentry.io/provider-finalizer"
	ClassFinalizer    = "agentry.io/class-finalizer"
	ChannelFinalizer  = "agentry.io/channel-finalizer"
)

// Well-known annotations and labels. See docs/src/controller/ and
// gateways/api/async-responses.md.
const (
	AnnotationWake                = "agentry.io/wake"                 // "true" triggers a wake on a Hibernated Agent
	AnnotationChannelDisconnected = "agentry.io/channel-disconnected" // "true", written by the gateway in the channel-delete handshake
	AnnotationExpiresAt           = "agentry.io/expires-at"           // RFC3339 TTL on async-response ConfigMaps (1h)

	LabelChannelNamespace = "agentry.io/channel-namespace" // on agentry-async-* ConfigMaps, for the label-selector sweep
	LabelChannelName      = "agentry.io/channel-name"
)

// SessionNamespaceUUID is the fixed UUIDv5 namespace used to derive an
// AgentChannel session id: sessionId = UUIDv5(SessionNamespaceUUID,
// channelId + ":" + userId). It is part of the published API and must never
// change after v1: changing it would invalidate existing session state in agent
// PVCs. See docs/src/gateways/api/agent-endpoints.md.
const SessionNamespaceUUID = "f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d"

// Certificate SAN suffixes used to identify a workload and extract its namespace
// (the label at index 1). The exact label count is enforced as defense against
// dotted-name spoofing. See docs/src/gateways/llm/workload-identity.md.
const (
	AgentSANSuffix = "svc.cluster.local" // {name}.{namespace}.svc.cluster.local, 5 labels
	TaskSANSuffix  = "task.agentry.io"   // {taskName}.{namespace}.task.agentry.io, 4 labels
	AgentSANLabels = 5
	TaskSANLabels  = 4
)
