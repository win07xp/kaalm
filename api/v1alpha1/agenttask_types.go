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

// AgentTaskSpec is a developer-owned run-to-completion workload. Artifacts may be
// declared only for agentReported completion (rule 17). See
// docs/src/resources/agenttask.md.
// +kubebuilder:validation:XValidation:rule="!(has(self.artifacts) && has(self.completion) && has(self.completion.condition) && self.completion.condition == 'exitCode')",message="artifacts require completion.condition: agentReported; an exitCode task cannot collect artifacts"
type AgentTaskSpec struct {
	// AgentClassRef selects the governing AgentClass.
	// +kubebuilder:validation:Required
	AgentClassRef LocalObjectReference `json:"agentClassRef"`
	// Image overrides the class default image (subject to the class allowlist).
	// +optional
	Image string `json:"image,omitempty"`
	// Env are extra environment variables merged with the injected KAALM_* set.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Providers lists the ModelProviders this task may call.
	// +optional
	Providers []AgentProviderReference `json:"providers,omitempty"`
	// Resources requested by the task container, clamped to the class maximum.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Persistence requests a PVC for the duration of the task.
	// +optional
	Persistence AgentTaskPersistence `json:"persistence,omitempty"`
	// Completion defines how the task reports done and its timeout behavior.
	// +optional
	Completion AgentTaskCompletion `json:"completion,omitempty"`
	// Artifacts declares the named outputs an agentReported task may return.
	// +optional
	Artifacts []AgentTaskArtifact `json:"artifacts,omitempty"`
	// TTLSecondsAfterFinished bounds how long a terminal task is retained.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// AgentTaskPersistence requests a PVC. Unlike Agent, there is no existingClaim.
type AgentTaskPersistence struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	SizeGi *int32 `json:"sizeGi,omitempty"`
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// AgentTaskCompletion defines completion detection and timeout behavior.
type AgentTaskCompletion struct {
	// Condition selects how completion is detected. agentReported uses
	// POST /v1/task/complete; exitCode uses container exit.
	// +kubebuilder:validation:Enum=agentReported;exitCode
	// +kubebuilder:default=agentReported
	// +optional
	Condition string `json:"condition,omitempty"`
	// Timeout bounds the task's running time.
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`
	// OnTimeout decides the terminal phase when the timeout fires.
	// +kubebuilder:validation:Enum=Fail;Succeed
	// +kubebuilder:default=Fail
	// +optional
	OnTimeout string `json:"onTimeout,omitempty"`
	// BackoffLimit is the number of Pod recreation retries before Failed.
	// +optional
	BackoffLimit int32 `json:"backoffLimit,omitempty"`
}

// AgentTaskArtifact declares a named output.
type AgentTaskArtifact struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentTaskStatus is the observed state of an AgentTask.
type AgentTaskStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the current lifecycle phase.
	// +optional
	Phase AgentTaskPhase `json:"phase,omitempty"`
	// Conditions report Completed.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	PodName string `json:"podName,omitempty"`
	// CurrentPodUID gates completion writes to the live task Pod (identity gate).
	// +optional
	CurrentPodUID string `json:"currentPodUID,omitempty"`
	// Retries counts Pod recreations under backoffLimit.
	// +optional
	Retries int32 `json:"retries,omitempty"`
	// ArtifactValues holds the collected agentReported outputs.
	// +optional
	ArtifactValues map[string]string `json:"artifactValues,omitempty"`
	// AgentReportedStatus is the status the agent reported at completion.
	// +kubebuilder:validation:Enum=success;failure
	// +optional
	AgentReportedStatus string `json:"agentReportedStatus,omitempty"`
	// +optional
	AgentReportedMessage string `json:"agentReportedMessage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=at
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="metadata.name must be a DNS-1123 label: lowercase alphanumerics and hyphens, no dots, at most 63 characters"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.agentClassRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentTask is the Schema for the agenttasks API.
type AgentTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentTaskSpec   `json:"spec,omitempty"`
	Status AgentTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentTaskList contains a list of AgentTask.
type AgentTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentTask{}, &AgentTaskList{})
}
