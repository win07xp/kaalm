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

// LocalObjectReference names another Kaalm resource by name, in the same scope
// as the referrer (same namespace for namespaced kinds, cluster-wide for
// cluster-scoped kinds). Used for agentClassRef, providerRef, allowedProviders
// entries, fallback entries, and agentRef.
type LocalObjectReference struct {
	// Name of the referenced object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretKeyReference points at a single key within a Secret. The Secret lives in
// the namespace appropriate to the referrer: kaalm-system for ModelProvider
// credentials, the AgentChannel's own namespace for channel auth Secrets.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key within the Secret's data.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}
