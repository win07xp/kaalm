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
	"fmt"
	"strings"

	"github.com/win07xp/kubeclaw/internal/gateway"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

const (
	// gatewayServiceAccount is the gateway's ServiceAccount name; the per-task
	// completion Role is bound to it.
	gatewayServiceAccount = "agentry-gateway"

	// Completion mailbox data layout, canonical in the gateway package (the
	// gateway writes these keys on POST /v1/task/complete; the reconciler
	// reads them). An empty mailbox has no status key.
	completionKeyStatus      = gateway.CompletionKeyStatus
	completionKeyMessage     = gateway.CompletionKeyMessage
	completionArtifactPrefix = gateway.CompletionArtifactPrefix

	// Completion condition and onTimeout enum values.
	completionAgentReported = "agentReported"
	completionExitCode      = "exitCode"
	onTimeoutSucceed        = "Succeed"

	// Reported status values on the wire and in status.agentReportedStatus.
	completionStatusSuccess = gateway.CompletionStatusSuccess
)

// effectiveTaskSpec is the AgentTask spec after merging AgentClass defaults at
// reconcile time. Same rules as effectiveAgentSpec, minus the Service and
// lifecycle blocks tasks do not have.
type effectiveTaskSpec struct {
	Image            string
	Env              []corev1.EnvVar
	Resources        corev1.ResourceRequirements
	HealthPort       int32
	PersistenceOn    bool
	PVCSizeGi        int32
	MountPath        string
	RuntimeClassName *string
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	PodSecurity      *corev1.PodSecurityContext
	ContainerSec     *corev1.SecurityContext
	TerminationGrace *int64
	PodLabels        map[string]string
	PodAnnotations   map[string]string
	Providers        []string
}

func deriveEffectiveTaskSpec(task *agentryv1alpha1.AgentTask, class *agentryv1alpha1.AgentClass) effectiveTaskSpec {
	eff := effectiveTaskSpec{
		Image:            task.Spec.Image,
		Env:              task.Spec.Env,
		Resources:        task.Spec.Resources,
		HealthPort:       defaultHealthPort,
		PersistenceOn:    task.Spec.Persistence.Enabled,
		MountPath:        task.Spec.Persistence.MountPath,
		RuntimeClassName: class.Spec.Runtime.RuntimeClassName,
		PullPolicy:       class.Spec.Image.PullPolicy,
		ImagePullSecrets: class.Spec.Image.ImagePullSecrets,
		PodSecurity:      class.Spec.Security.PodSecurityContext,
		ContainerSec:     class.Spec.Security.ContainerSecurityContext,
		TerminationGrace: class.Spec.Lifecycle.TerminationGracePeriodSeconds,
		PodLabels:        class.Spec.PodMetadata.Labels,
		PodAnnotations:   class.Spec.PodMetadata.Annotations,
	}
	if eff.Image == "" {
		eff.Image = class.Spec.Image.DefaultImage
	}
	if len(eff.Resources.Requests) == 0 && len(eff.Resources.Limits) == 0 {
		eff.Resources = class.Spec.Resources.Defaults
	}
	eff.Resources = clampResources(eff.Resources, class.Spec.Resources.MaxLimits)
	if task.Spec.Persistence.SizeGi != nil {
		eff.PVCSizeGi = *task.Spec.Persistence.SizeGi
	} else {
		eff.PVCSizeGi = class.Spec.Persistence.DefaultSizeGi
	}
	if max := class.Spec.Persistence.MaxSizeGi; max > 0 && eff.PVCSizeGi > max {
		eff.PVCSizeGi = max
	}
	for _, p := range task.Spec.Providers {
		eff.Providers = append(eff.Providers, p.ProviderRef.Name)
	}
	return eff
}

func taskCertificateName(taskName string) string    { return taskName + "-tls" }
func taskPVCName(taskName string) string            { return taskName + "-workspace" }
func taskServiceAccountName(taskName string) string { return "task-" + taskName }
func taskCompletionCMName(taskName string) string   { return taskName + "-completion" }
func taskCompletionRoleName(taskName string) string {
	return "agentry-task-" + taskName + "-completion"
}

func taskPodLabels(task *agentryv1alpha1.AgentTask) map[string]string {
	return map[string]string{
		"agentry.io/task":     task.Name,
		"agentry.io/workload": "task",
	}
}

// isAgentReported reports whether the task uses agentReported completion (the
// CRD default when the block or field is absent).
func isAgentReported(task *agentryv1alpha1.AgentTask) bool {
	return task.Spec.Completion.Condition != completionExitCode
}

// desiredTaskCertificate is the per-task client certificate: a single SAN in
// the non-Service task shape and client auth only, since tasks have no inbound
// listener. See docs/src/security/tls.md.
func desiredTaskCertificate(task *agentryv1alpha1.AgentTask) *cmapi.Certificate {
	return &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: taskCertificateName(task.Name), Namespace: task.Namespace},
		Spec: cmapi.CertificateSpec{
			SecretName: taskCertificateName(task.Name),
			IssuerRef:  cmmeta.ObjectReference{Name: clusterIssuerName, Kind: "ClusterIssuer"},
			DNSNames: []string{
				fmt.Sprintf("%s.%s.%s", task.Name, task.Namespace, agentryv1alpha1.TaskSANSuffix),
			},
			Duration:    &metav1.Duration{Duration: certDuration},
			RenewBefore: &metav1.Duration{Duration: certRenewBefore},
			Usages:      []cmapi.KeyUsage{cmapi.UsageClientAuth},
		},
	}
}

func desiredTaskServiceAccount(task *agentryv1alpha1.AgentTask) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: taskServiceAccountName(task.Name), Namespace: task.Namespace},
	}
}

func desiredTaskPVC(task *agentryv1alpha1.AgentTask, class *agentryv1alpha1.AgentClass, eff effectiveTaskSpec) *corev1.PersistentVolumeClaim {
	size := eff.PVCSizeGi
	if size <= 0 {
		size = 1
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: taskPVCName(task.Name), Namespace: task.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: class.Spec.Persistence.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(int64(size)<<30, resource.BinarySI),
				},
			},
		},
	}
}

// desiredCompletionConfigMap is the empty completion mailbox, pre-created so
// the gateway's name-scoped update/patch Role is enforceable (RBAC
// resourceNames cannot constrain create).
func desiredCompletionConfigMap(task *agentryv1alpha1.AgentTask) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: taskCompletionCMName(task.Name), Namespace: task.Namespace},
		Data:       map[string]string{},
	}
}

// desiredCompletionRole grants the gateway update and patch, and nothing else,
// on exactly the completion ConfigMap. No create: resourceNames cannot scope
// it, so granting it would broaden access to every ConfigMap in the namespace.
func desiredCompletionRole(task *agentryv1alpha1.AgentTask) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: taskCompletionRoleName(task.Name), Namespace: task.Namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{taskCompletionCMName(task.Name)},
			Verbs:         []string{"update", "patch"},
		}},
	}
}

func desiredCompletionRoleBinding(task *agentryv1alpha1.AgentTask, operatorNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: taskCompletionRoleName(task.Name), Namespace: task.Namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     taskCompletionRoleName(task.Name),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      gatewayServiceAccount,
			Namespace: operatorNamespace,
		}},
	}
}

// desiredTaskPod derives the task Pod: restartPolicy Never (Agentry owns
// retries via backoffLimit, and exitCode completion depends on terminal Pod
// phases), no kubelet probes, and the same injected env and TLS volume as an
// Agent Pod.
func desiredTaskPod(task *agentryv1alpha1.AgentTask, eff effectiveTaskSpec, operatorNamespace string) *corev1.Pod {
	labels := map[string]string{}
	for k, v := range eff.PodLabels {
		labels[k] = v
	}
	for k, v := range taskPodLabels(task) {
		labels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range eff.PodAnnotations {
		annotations[k] = v
	}

	env := []corev1.EnvVar{
		{Name: "AGENTRY_HEALTH_PORT", Value: fmt.Sprintf("%d", eff.HealthPort)},
		{Name: "AGENTRY_GATEWAY_ENDPOINT", Value: gatewayEndpoint(operatorNamespace)},
		{Name: "AGENTRY_CA_CERT", Value: tlsMountPath + "/ca.crt"},
		{Name: "AGENTRY_TLS_CERT", Value: tlsMountPath + "/tls.crt"},
		{Name: "AGENTRY_TLS_KEY", Value: tlsMountPath + "/tls.key"},
	}
	env = append(env, eff.Env...)

	volumes := []corev1.Volume{{
		Name: "agentry-tls",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: taskCertificateName(task.Name)},
						Items: []corev1.KeyToPath{
							{Key: "tls.crt", Path: "tls.crt"},
							{Key: "tls.key", Path: "tls.key"},
						},
					}},
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: caBundleConfigMap},
						Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
					}},
				},
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: "agentry-tls", MountPath: tlsMountPath, ReadOnly: true}}

	if eff.PersistenceOn {
		mountPath := eff.MountPath
		if mountPath == "" {
			mountPath = "/var/task/workspace"
		}
		volumes = append(volumes, corev1.Volume{
			Name: "task-workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: taskPVCName(task.Name)},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "task-workspace", MountPath: mountPath})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: task.Name + "-",
			Namespace:    task.Namespace,
			Labels:       labels,
			Annotations:  annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ServiceAccountName:            taskServiceAccountName(task.Name),
			RuntimeClassName:              eff.RuntimeClassName,
			ImagePullSecrets:              eff.ImagePullSecrets,
			SecurityContext:               eff.PodSecurity,
			TerminationGracePeriodSeconds: eff.TerminationGrace,
			Containers: []corev1.Container{{
				Name:            agentContainer,
				Image:           eff.Image,
				Env:             env,
				Resources:       eff.Resources,
				ImagePullPolicy: eff.PullPolicy,
				SecurityContext: eff.ContainerSec,
				VolumeMounts:    mounts,
			}},
			Volumes: volumes,
		},
	}
}

// desiredTaskNetworkPolicy mirrors the Agent policy minus every ingress rule:
// tasks have no listener and are not delivery targets. ingress stays an
// explicit empty list to document deny-all intent.
func desiredTaskNetworkPolicy(
	task *agentryv1alpha1.AgentTask, class *agentryv1alpha1.AgentClass, operatorNamespace string,
) *networkingv1.NetworkPolicy {
	protoTCP := corev1.ProtocolTCP
	protoUDP := corev1.ProtocolUDP
	gwPort := intstr.FromInt32(gatewayPort)
	dnsPort := intstr.FromInt32(53)

	gatewayPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/component": "gateway"},
		},
	}
	dnsPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		},
	}

	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{gatewayPeer}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protoTCP, Port: &gwPort}}},
		{To: []networkingv1.NetworkPolicyPeer{dnsPeer}, Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &protoUDP, Port: &dnsPort}, {Protocol: &protoTCP, Port: &dnsPort},
		}},
	}
	for _, cidr := range class.Spec.Network.Egress.AllowedCIDRs {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: task.Name, Namespace: task.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: taskPodLabels(task)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      egress,
		},
	}
}

// completionPayload is the parsed mailbox content.
type completionPayload struct {
	Status    string // "success" | "failure"; empty means no completion yet
	Message   string
	Artifacts map[string]string
}

// parseCompletion reads the mailbox data layout. An absent status key means
// the mailbox is empty (no completion reported).
func parseCompletion(data map[string]string) completionPayload {
	p := completionPayload{
		Status:    data[completionKeyStatus],
		Message:   data[completionKeyMessage],
		Artifacts: map[string]string{},
	}
	for k, v := range data {
		if name, ok := strings.CutPrefix(k, completionArtifactPrefix); ok {
			p.Artifacts[name] = v
		}
	}
	return p
}

// validateArtifactNames applies the per-status rule from the wire contract:
// strict (all declared present, none undeclared) on success; no-undeclared
// only on failure. Returns "" when valid, else a message naming the offender.
func validateArtifactNames(p completionPayload, declared []agentryv1alpha1.AgentTaskArtifact) string {
	names := map[string]bool{}
	for _, a := range declared {
		names[a.Name] = true
	}
	for k := range p.Artifacts {
		if !names[k] {
			return "undeclared artifact in payload: " + k
		}
	}
	if p.Status == completionStatusSuccess {
		for _, a := range declared {
			if _, ok := p.Artifacts[a.Name]; !ok {
				return "missing declared artifact: " + a.Name
			}
		}
	}
	return ""
}
