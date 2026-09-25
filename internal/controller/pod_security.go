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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The restricted Pod Security Standard is the default for every workload Pod
// (docs/src/security/model.md, Pod Security Standards). A class overrides it
// per field; a field the class leaves unset takes the baseline value.
const (
	tmpVolumeName = "tmp"
	tmpMountPath  = "/tmp"
)

// restrictedPodSecurity returns the class's Pod security context with the
// restricted baseline filled into every unset field. The declared context is
// never mutated.
func restrictedPodSecurity(declared *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{}
	if declared != nil {
		sc = declared.DeepCopy()
	}
	if sc.RunAsNonRoot == nil {
		sc.RunAsNonRoot = boolPtr(true)
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	return sc
}

// restrictedContainerSecurity returns the class's container security context
// with the restricted baseline filled into every unset field.
func restrictedContainerSecurity(declared *corev1.SecurityContext) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{}
	if declared != nil {
		sc = declared.DeepCopy()
	}
	if sc.AllowPrivilegeEscalation == nil {
		sc.AllowPrivilegeEscalation = boolPtr(false)
	}
	if sc.ReadOnlyRootFilesystem == nil {
		sc.ReadOnlyRootFilesystem = boolPtr(true)
	}
	if sc.Capabilities == nil {
		sc.Capabilities = &corev1.Capabilities{}
	}
	if sc.Capabilities.Drop == nil {
		sc.Capabilities.Drop = []corev1.Capability{"ALL"}
	}
	return sc
}

// tmpVolume is the emptyDir mounted at /tmp whenever the container root is
// read-only, so images that write temp files work without a PVC.
func tmpVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{
		Name:         tmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, corev1.VolumeMount{Name: tmpVolumeName, MountPath: tmpMountPath}
}

// readOnlyRoot reports whether the effective container context makes the
// root filesystem read-only.
func readOnlyRoot(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem
}

// securityBaselineDeviations lists every declared field that falls below the
// restricted baseline. An unset field is not a deviation: it takes the
// baseline value.
func securityBaselineDeviations(sec kaalmv1beta1.AgentClassSecurity) []string {
	var out []string
	if p := sec.PodSecurityContext; p != nil {
		if p.RunAsNonRoot != nil && !*p.RunAsNonRoot {
			out = append(out, "podSecurityContext.runAsNonRoot is false")
		}
		if p.RunAsUser != nil && *p.RunAsUser == 0 {
			out = append(out, "podSecurityContext.runAsUser is 0")
		}
		if p.SeccompProfile != nil && p.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
			out = append(out, "podSecurityContext.seccompProfile.type is Unconfined")
		}
	}
	if c := sec.ContainerSecurityContext; c != nil {
		if c.Privileged != nil && *c.Privileged {
			out = append(out, "containerSecurityContext.privileged is true")
		}
		if c.AllowPrivilegeEscalation != nil && *c.AllowPrivilegeEscalation {
			out = append(out, "containerSecurityContext.allowPrivilegeEscalation is true")
		}
		if c.ReadOnlyRootFilesystem != nil && !*c.ReadOnlyRootFilesystem {
			out = append(out, "containerSecurityContext.readOnlyRootFilesystem is false")
		}
		if c.RunAsNonRoot != nil && !*c.RunAsNonRoot {
			out = append(out, "containerSecurityContext.runAsNonRoot is false")
		}
		if c.RunAsUser != nil && *c.RunAsUser == 0 {
			out = append(out, "containerSecurityContext.runAsUser is 0")
		}
		if c.SeccompProfile != nil && c.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
			out = append(out, "containerSecurityContext.seccompProfile.type is Unconfined")
		}
		if c.Capabilities != nil {
			for _, add := range c.Capabilities.Add {
				if add != "NET_BIND_SERVICE" {
					out = append(out, fmt.Sprintf("containerSecurityContext.capabilities.add includes %s", add))
				}
			}
		}
	}
	return out
}

func boolPtr(b bool) *bool { return &b }
