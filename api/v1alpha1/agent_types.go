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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSpec is a developer-owned persistent agent workload governed by an
// AgentClass. Only agentClassRef is required. See docs/src/resources/agent.md.
type AgentSpec struct {
	// AgentClassRef selects the governing AgentClass.
	// +kubebuilder:validation:Required
	AgentClassRef LocalObjectReference `json:"agentClassRef"`
	// Image overrides the class default image (subject to the class allowlist).
	// +optional
	Image string `json:"image,omitempty"`
	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`
	// Args overrides the container args.
	// +optional
	Args []string `json:"args,omitempty"`
	// Env are extra environment variables merged with the injected AGENTRY_* set.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Providers lists the ModelProviders this agent may call. Omit for agents
	// that make no LLM calls.
	// +optional
	Providers []AgentProviderReference `json:"providers,omitempty"`
	// Resources requested by the agent container, clamped to the class maximum.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Persistence requests a PVC for durable agent state.
	// +optional
	Persistence AgentPersistence `json:"persistence,omitempty"`
	// Lifecycle configures idle, hibernation, and wake behavior.
	// +optional
	Lifecycle AgentLifecycle `json:"lifecycle,omitempty"`
	// Service exposes the agent's HTTPS endpoint for intra-cluster delivery.
	// A nil block defaults to an enabled Service on port 8080 at reconcile
	// time (the CRD default on enabled only fires when the block is present).
	// +optional
	Service *AgentService `json:"service,omitempty"`
	// MCPServers are referenced only for NetworkPolicy egress scoping.
	// +optional
	MCPServers []AgentMCPServer `json:"mcpServers,omitempty"`
}

// AgentProviderReference names a ModelProvider the agent may use.
type AgentProviderReference struct {
	// ProviderRef names the ModelProvider.
	// +kubebuilder:validation:Required
	ProviderRef LocalObjectReference `json:"providerRef"`
}

// AgentPersistence requests durable state. sizeGi and existingClaim are mutually
// exclusive (rule 27a): a pre-existing claim brings its own size.
// +kubebuilder:validation:XValidation:rule="!(has(self.sizeGi) && has(self.existingClaim))",message="sizeGi and existingClaim are mutually exclusive"
type AgentPersistence struct {
	// Enabled requests a persistent volume.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// SizeGi is the requested volume size, clamped to the class maximum. Omit
	// when using existingClaim.
	// +optional
	SizeGi *int32 `json:"sizeGi,omitempty"`
	// MountPath is where the volume is mounted in the agent container.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
	// ExistingClaim references a pre-provisioned PVC by name. The controller does
	// not own or provision it.
	// +optional
	ExistingClaim *string `json:"existingClaim,omitempty"`
}

// AgentLifecycle configures idle, hibernation, and wake.
type AgentLifecycle struct {
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitempty"`
	// HibernationEnabled deletes the Pod (keeping the PVC) after the hibernation
	// delay, and recreates it on the next inbound message. Requires persistence.
	// +optional
	HibernationEnabled bool `json:"hibernationEnabled,omitempty"`
	// +optional
	HibernationDelay metav1.Duration `json:"hibernationDelay,omitempty"`
	// ActivitySource selects which signals count as activity for idle detection.
	// +kubebuilder:validation:Enum=gatewayTraffic;agentHeartbeat;both
	// +kubebuilder:default=gatewayTraffic
	// +optional
	ActivitySource string `json:"activitySource,omitempty"`
	// +optional
	WakeTimeout metav1.Duration `json:"wakeTimeout,omitempty"`
}

// AgentService exposes the agent's HTTPS listener.
type AgentService struct {
	// Enabled creates a ClusterIP Service. An agent with the Service disabled is
	// outbound-only and cannot be targeted by an AgentChannel.
	// The json tag deliberately has no omitempty: a serialized explicit
	// false must survive the wire, or the CRD default would overwrite it.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`
	// Port is the Service port exposing the agent's health/message listener.
	// +optional
	Port int32 `json:"port,omitempty"`
}

// AgentMCPServer is an MCP endpoint the agent may reach (egress scoping only).
type AgentMCPServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// AgentStatus is the observed state of an Agent.
type AgentStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the current lifecycle phase.
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`
	// Conditions report Ready, ProvidersReady, and the recoverable Degraded state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Endpoint is the in-cluster HTTPS URL for the agent, when the Service exists.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// +optional
	LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`
	// +optional
	PhaseTransitionTime *metav1.Time `json:"phaseTransitionTime,omitempty"`
	// +optional
	HibernatedAt *metav1.Time `json:"hibernatedAt,omitempty"`
	// PreDegradedPhase records the phase to restore when a Degraded condition
	// clears.
	// +optional
	PreDegradedPhase AgentPhase `json:"preDegradedPhase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="metadata.name must be a DNS-1123 label: lowercase alphanumerics and hyphens, no dots, at most 63 characters"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.agentClassRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the Schema for the agents API.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
