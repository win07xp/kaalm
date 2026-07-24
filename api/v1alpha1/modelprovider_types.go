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

// ModelProviderSpec is a platform-owned wrapper around one LLM provider: its
// endpoint, credentials, model catalog, tenancy, budget, and fallback tree.
// See docs/src/resources/modelprovider.md.
type ModelProviderSpec struct {
	// Type selects the provider protocol.
	// +kubebuilder:validation:Enum=anthropic;openai;google-vertex;openai-compatible
	// +kubebuilder:validation:Required
	Type string `json:"type"`
	// Endpoint is the provider base URL. Must be HTTPS.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	Endpoint string `json:"endpoint"`
	// CredentialsRef points at the Secret key holding the provider credential,
	// read by the gateway in the operator namespace.
	// +kubebuilder:validation:Required
	CredentialsRef SecretKeyReference `json:"credentialsRef"`
	// Models is the catalog this provider serves, keyed by id.
	// +listType=map
	// +listMapKey=id
	// +optional
	Models []ModelProviderModel `json:"models,omitempty"`
	// AllowedNamespaces are glob patterns; a caller's namespace must match one.
	// "*" allows all; empty allows none.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
	// Budget sets soft spend guardrails.
	// +optional
	Budget ModelProviderBudget `json:"budget,omitempty"`
	// RateLimits set the cluster-wide request and token ceilings.
	// +optional
	RateLimits ModelProviderRateLimits `json:"rateLimits,omitempty"`
	// Fallback names providers tried when this one fails. Each may declare its
	// own fallback, forming a tree walked depth-first. Must be acyclic and share
	// this provider's type.
	// +optional
	Fallback []LocalObjectReference `json:"fallback,omitempty"`
	// HealthCheck configures the periodic upstream liveness probe. A nil block
	// defaults to an enabled probe at reconcile time (the CRD default on enabled
	// only fires when the block is present).
	// +optional
	HealthCheck *ModelProviderHealthCheck `json:"healthCheck,omitempty"`
}

// ModelProviderModel is one entry in the catalog. Costs are decimal USD strings
// per one million tokens.
type ModelProviderModel struct {
	// ID is the model identifier as sent to the provider.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
	// DisplayName is a human-readable label.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// CostPer1MInputTokens is USD per million input tokens, as a decimal string.
	// +optional
	CostPer1MInputTokens string `json:"costPer1MInputTokens,omitempty"`
	// CostPer1MOutputTokens is USD per million output tokens, as a decimal string.
	// +optional
	CostPer1MOutputTokens string `json:"costPer1MOutputTokens,omitempty"`
}

// ModelProviderBudget sets soft spend guardrails.
type ModelProviderBudget struct {
	// Period is the budget accounting window.
	// +kubebuilder:validation:Enum=monthly;weekly;daily;none
	// +kubebuilder:default=none
	// +optional
	Period string `json:"period,omitempty"`
	// PerNamespaceUSD is the per-namespace spend ceiling for the period, as a
	// decimal string.
	// +optional
	PerNamespaceUSD string `json:"perNamespaceUSD,omitempty"`
	// Policies fire actions as spend crosses thresholds.
	// +optional
	Policies []ModelProviderBudgetPolicy `json:"policies,omitempty"`
	// ClusterUSD is an optional cluster-wide spend ceiling for the period.
	// +optional
	ClusterUSD *string `json:"clusterUSD,omitempty"`
}

// ModelProviderBudgetPolicy is a threshold action.
type ModelProviderBudgetPolicy struct {
	// AtPercent is the utilization percentage at which the action fires.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	AtPercent int32 `json:"atPercent"`
	// Action taken at the threshold.
	// +kubebuilder:validation:Enum=block;warn;degrade
	Action string `json:"action"`
	// DegradeTo names a cheaper model id in this provider's catalog. Required
	// when action is degrade; validated at reconcile time (rule 18).
	// +optional
	DegradeTo *string `json:"degradeTo,omitempty"`
}

// ModelProviderRateLimits set cluster-wide ceilings.
type ModelProviderRateLimits struct {
	// +optional
	RequestsPerMinute int32 `json:"requestsPerMinute,omitempty"`
	// +optional
	TokensPerMinute int32 `json:"tokensPerMinute,omitempty"`
}

// ModelProviderHealthCheck configures the liveness probe.
type ModelProviderHealthCheck struct {
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

// ModelProviderStatus is the observed state of a ModelProvider.
type ModelProviderStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions report Ready and Healthy.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// BudgetUsage is per-namespace spend for the current period.
	// +optional
	BudgetUsage []ModelProviderBudgetUsage `json:"budgetUsage,omitempty"`
	// ClusterSpentUSD is total spend across namespaces for the period.
	// +optional
	ClusterSpentUSD string `json:"clusterSpentUSD,omitempty"`
}

// ModelProviderBudgetUsage is per-namespace spend.
type ModelProviderBudgetUsage struct {
	Namespace string `json:"namespace"`
	Period    string `json:"period"`
	// SpentUSD is spend so far this period, as a decimal string.
	SpentUSD string `json:"spentUSD"`
	// PercentUsed is spend against the per-namespace ceiling.
	PercentUsed int32 `json:"percentUsed"`
	// State is the enforcement state for this namespace.
	// +kubebuilder:validation:Enum=Normal;Throttled;Blocked
	State string `json:"state"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mp
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Healthy",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelProvider is the Schema for the modelproviders API.
type ModelProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelProviderSpec   `json:"spec,omitempty"`
	Status ModelProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelProviderList contains a list of ModelProvider.
type ModelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelProvider{}, &ModelProviderList{})
}
