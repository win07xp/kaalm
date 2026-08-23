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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolProviderSpec is a platform-owned wrapper around one external tool
// server: its protocol, endpoint, credentials, tool catalog, and tenancy.
// It deliberately rhymes with ModelProviderSpec; the gateway brokers tool
// calls the way it proxies LLM calls. See docs/src/resources/toolprovider.md
// and docs/src/gateways/tool-plane.md.
type ToolProviderSpec struct {
	// Type selects the tool protocol. v0.4.0 supports exactly "mcp"
	// (MCP streamable HTTP); the slot exists so a future protocol is an
	// addition, not a reshape.
	// +kubebuilder:validation:Enum=mcp
	// +kubebuilder:validation:Required
	Type string `json:"type"`
	// Endpoint is the tool server base URL. Must be HTTPS; in-cluster and
	// external endpoints are equally valid.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	Endpoint string `json:"endpoint"`
	// CredentialsRef points at the Secret key holding the server credential,
	// resolved only from the operator namespace, never from a tenant
	// namespace. Omit it for servers that require no authentication.
	// +optional
	CredentialsRef *SecretKeyReference `json:"credentialsRef,omitempty"`
	// AllowedNamespaces are glob patterns; a caller's namespace must match one.
	// "*" allows all; empty allows none.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
	// Tools is the optional declared catalog, keyed by id. When present it is
	// a ceiling: the broker rejects calls to uncataloged tools, grants are
	// validated against it (rule 38), and the audit metric's tool label is
	// bounded by it. When omitted, the server's own tools/list governs.
	// +listType=map
	// +listMapKey=id
	// +optional
	Tools []ToolProviderTool `json:"tools,omitempty"`
	// RateLimits sets the per-namespace request ceiling for brokered calls.
	// The configured value is a cluster-wide ceiling; each gateway replica
	// divides it by the live replica count, exactly as ModelProvider rate
	// limits are enforced. Zero or omitted means no limit.
	// +optional
	RateLimits ToolProviderRateLimits `json:"rateLimits,omitempty"`
	// HealthCheck configures the periodic upstream liveness probe. A nil block
	// defaults to an enabled probe at reconcile time (the CRD default on enabled
	// only fires when the block is present).
	// +optional
	HealthCheck *ToolProviderHealthCheck `json:"healthCheck,omitempty"`
}

// ToolProviderRateLimits sets the brokered-call ceiling. Tool calls carry no
// token dimension, so unlike ModelProvider there is only a request rate.
type ToolProviderRateLimits struct {
	// +optional
	RequestsPerMinute int32 `json:"requestsPerMinute,omitempty"`
}

// ToolProviderTool is one entry in the declared catalog.
type ToolProviderTool struct {
	// ID is the tool name as the server exposes it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
}

// ToolProviderHealthCheck configures the liveness probe. The probe speaks the
// protocol it governs: an MCP initialize followed by tools/list.
type ToolProviderHealthCheck struct {
	// Enabled runs the periodic upstream liveness probe. Set false to disable it.
	// The json tag deliberately has no omitempty: a serialized explicit false
	// must survive the wire, or the CRD default would overwrite it.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// ToolProviderStatus is the observed state of a ToolProvider.
type ToolProviderStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions report Ready and Healthy.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:deprecatedversion:warning="kaalm.io/v1alpha1 ToolProvider is deprecated; use kaalm.io/v1beta1"
// +kubebuilder:resource:scope=Cluster,shortName=tp
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Healthy",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ToolProvider is the Schema for the toolproviders API.
type ToolProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolProviderSpec   `json:"spec,omitempty"`
	Status ToolProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ToolProviderList contains a list of ToolProvider.
type ToolProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolProvider{}, &ToolProviderList{})
}
