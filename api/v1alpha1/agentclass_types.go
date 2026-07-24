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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentClassSpec is a platform-owned policy template that governs how a category
// of Agents and AgentTasks may run. See docs/src/resources/agentclass.md.
type AgentClassSpec struct {
	// Runtime selects the workload backend and container runtime class.
	// +optional
	Runtime AgentClassRuntime `json:"runtime,omitempty"`
	// Image constrains which images this class permits and how they are pulled.
	// +optional
	Image AgentClassImage `json:"image,omitempty"`
	// Resources sets the default and maximum compute for workloads of this class.
	// +optional
	Resources AgentClassResources `json:"resources,omitempty"`
	// Persistence governs PVC provisioning for workloads of this class.
	// +optional
	Persistence AgentClassPersistence `json:"persistence,omitempty"`
	// AllowedProviders lists the ModelProviders workloads of this class may use.
	// An empty list allows none.
	// +optional
	AllowedProviders []LocalObjectReference `json:"allowedProviders,omitempty"`
	// Network governs egress and ingress policy synthesized per workload.
	// +optional
	Network AgentClassNetwork `json:"network,omitempty"`
	// Security sets the Pod and container security contexts applied to workloads.
	// +optional
	Security AgentClassSecurity `json:"security,omitempty"`
	// Lifecycle sets idle, hibernation, and wake defaults and ceilings.
	// +optional
	Lifecycle AgentClassLifecycle `json:"lifecycle,omitempty"`
	// PodMetadata is merged onto every workload Pod's labels and annotations.
	// +optional
	PodMetadata AgentClassPodMetadata `json:"podMetadata,omitempty"`
}

// AgentClassRuntime selects the workload backend.
type AgentClassRuntime struct {
	// Backend is the workload backend. Only "pod" is supported in v1.
	// +kubebuilder:validation:Enum=pod
	// +kubebuilder:default=pod
	// +optional
	Backend string `json:"backend,omitempty"`
	// RuntimeClassName, if set, is applied to workload Pods (for example a gVisor
	// or Kata class). Unset runs under the cluster default runtime.
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
}

// AgentClassImage constrains permitted images.
type AgentClassImage struct {
	// AllowedImages is a list of path.Match glob patterns. A workload image must
	// match at least one when the list is non-empty.
	// +optional
	AllowedImages []string `json:"allowedImages,omitempty"`
	// DefaultImage is applied at reconcile time when a workload omits its image.
	// +optional
	DefaultImage string `json:"defaultImage,omitempty"`
	// PullPolicy is the image pull policy for workload containers.
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
	// ImagePullSecrets are attached to workload Pods and must exist in the
	// workload's namespace.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// AgentClassResources sets default and ceiling compute.
type AgentClassResources struct {
	// Defaults are applied at reconcile time when a workload omits resources.
	// +optional
	Defaults corev1.ResourceRequirements `json:"defaults,omitempty"`
	// MaxLimits caps a workload's resource limits. Requests above the cap are
	// clamped at reconcile time.
	// +optional
	MaxLimits corev1.ResourceList `json:"maxLimits,omitempty"`
}

// AgentClassPersistence governs PVC provisioning.
type AgentClassPersistence struct {
	// Enabled permits workloads of this class to request persistence.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// DefaultSizeGi is applied at reconcile time when a workload omits a size.
	// +optional
	DefaultSizeGi int32 `json:"defaultSizeGi,omitempty"`
	// MaxSizeGi caps the provisioned volume size.
	// +optional
	MaxSizeGi int32 `json:"maxSizeGi,omitempty"`
	// StorageClassName is applied to provisioned PVCs.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// PVCRetention decides whether a provisioned PVC survives workload deletion.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	// +optional
	PVCRetention string `json:"pvcRetention,omitempty"`
}

// AgentClassNetwork governs synthesized NetworkPolicy.
type AgentClassNetwork struct {
	// Egress lists permitted external destinations beyond the gateway.
	// +optional
	Egress AgentClassEgress `json:"egress,omitempty"`
	// AllowHostNetwork permits workload Pods to use host networking.
	// +optional
	AllowHostNetwork bool `json:"allowHostNetwork,omitempty"`
	// AllowSameNamespaceIngress permits ingress from Pods in the same namespace.
	// +optional
	AllowSameNamespaceIngress bool `json:"allowSameNamespaceIngress,omitempty"`
}

// AgentClassEgress lists permitted egress.
type AgentClassEgress struct {
	// AllowedCIDRs are CIDR blocks permitted for egress, enforced on any CNI that
	// implements standard NetworkPolicy.
	// +optional
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`
	// AllowedHosts are DNS names permitted for egress, enforced only on CNIs that
	// support FQDN policies (Cilium, Calico Enterprise).
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

// AgentClassSecurity sets security contexts.
type AgentClassSecurity struct {
	// PodSecurityContext is applied to workload Pods.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// ContainerSecurityContext is applied to workload containers.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
}

// AgentClassLifecycle sets lifecycle defaults and ceilings. Timeouts are
// durations like "30m".
type AgentClassLifecycle struct {
	// +optional
	DefaultIdleTimeout metav1.Duration `json:"defaultIdleTimeout,omitempty"`
	// +optional
	MaxIdleTimeout metav1.Duration `json:"maxIdleTimeout,omitempty"`
	// HibernationAllowed permits workloads of this class to enable hibernation.
	// +optional
	HibernationAllowed bool `json:"hibernationAllowed,omitempty"`
	// +optional
	DefaultHibernationDelay metav1.Duration `json:"defaultHibernationDelay,omitempty"`
	// +optional
	MaxHibernationDelay metav1.Duration `json:"maxHibernationDelay,omitempty"`
	// +optional
	DefaultWakeTimeout metav1.Duration `json:"defaultWakeTimeout,omitempty"`
	// +optional
	MaxWakeTimeout metav1.Duration `json:"maxWakeTimeout,omitempty"`
	// TerminationGracePeriodSeconds is applied to workload Pods.
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// AgentClassPodMetadata is merged onto workload Pods.
type AgentClassPodMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// AgentClassStatus is the observed state of an AgentClass.
type AgentClassStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions report class validity (Ready) and CNI FQDN support.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// AgentsInUse counts Agents currently referencing this class.
	// +optional
	AgentsInUse int32 `json:"agentsInUse,omitempty"`
	// TasksInUse counts AgentTasks currently referencing this class.
	// +optional
	TasksInUse int32 `json:"tasksInUse,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ac
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agentsInUse`
// +kubebuilder:printcolumn:name="Tasks",type=integer,JSONPath=`.status.tasksInUse`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentClass is the Schema for the agentclasses API.
type AgentClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentClassSpec   `json:"spec,omitempty"`
	Status AgentClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentClassList contains a list of AgentClass.
type AgentClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentClass{}, &AgentClassList{})
}
