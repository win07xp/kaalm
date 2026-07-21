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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// Well-known names and defaults shared by the desired-state builders. The
// gateway Service name and ports come from the design (docs/src/gateways/); the
// mount path and env var names are the runtime contract (docs/src/runtime/).
const (
	gatewayServiceName = "kaalm-gateway"
	gatewayPort        = 8443
	defaultHealthPort  = int32(8080)
	tlsMountPath       = "/var/run/kaalm"
	caBundleConfigMap  = "kaalm-ca" // trust-manager Bundle target
	clusterIssuerName  = "kaalm-ca-issuer"

	// annotationPodSpecHash carries the derived-Pod-spec hash for drift
	// detection (the Deployment pod-template-hash idiom). Never compare
	// against the live Pod object: apiserver defaulting would report
	// perpetual drift.
	annotationPodSpecHash = "kaalm.io/pod-spec-hash"

	certDuration    = 2160 * time.Hour // 90d chart default
	certRenewBefore = 720 * time.Hour  // 30d
	agentContainer  = "agent"
)

// effectiveAgentSpec is the Agent spec after merging AgentClass-derived
// defaults at reconcile time. The stored Agent spec is never mutated: it keeps
// reflecting what the developer wrote (docs/src/resources/validation-and-defaulting.md).
type effectiveAgentSpec struct {
	Image            string
	Command          []string
	Args             []string
	Env              []corev1.EnvVar
	Resources        corev1.ResourceRequirements
	HealthPort       int32
	ServicePort      int32
	ServiceEnabled   bool
	PersistenceOn    bool
	PVCSizeGi        int32
	MountPath        string
	ExistingClaim    string
	RuntimeClassName *string
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	PodSecurity      *corev1.PodSecurityContext
	ContainerSec     *corev1.SecurityContext
	TerminationGrace *int64
	PodLabels        map[string]string
	PodAnnotations   map[string]string
	Providers        []string

	// Lifecycle knobs, defaulted from the class and capped by it.
	IdleTimeout        time.Duration
	HibernationDelay   time.Duration
	HibernationEnabled bool
	ActivitySource     string
}

// deriveEffectiveSpec merges class defaults into the Agent's spec and clamps
// resources to the class maximum.
func deriveEffectiveSpec(agent *kaalmv1alpha1.Agent, class *kaalmv1alpha1.AgentClass) effectiveAgentSpec {
	eff := effectiveAgentSpec{
		Image:            agent.Spec.Image,
		Command:          agent.Spec.Command,
		Args:             agent.Spec.Args,
		Env:              agent.Spec.Env,
		Resources:        agent.Spec.Resources,
		HealthPort:       defaultHealthPort,
		ServiceEnabled:   true,
		PersistenceOn:    agent.Spec.Persistence.Enabled,
		MountPath:        agent.Spec.Persistence.MountPath,
		RuntimeClassName: class.Spec.Runtime.RuntimeClassName,
		PullPolicy:       class.Spec.Image.PullPolicy,
		ImagePullSecrets: class.Spec.Image.ImagePullSecrets,
		PodSecurity:      class.Spec.Security.PodSecurityContext,
		ContainerSec:     class.Spec.Security.ContainerSecurityContext,
		TerminationGrace: class.Spec.Lifecycle.TerminationGracePeriodSeconds,
		PodLabels:        class.Spec.PodMetadata.Labels,
		PodAnnotations:   class.Spec.PodMetadata.Annotations,
	}
	if agent.Spec.Service != nil {
		eff.ServiceEnabled = agent.Spec.Service.Enabled
		eff.ServicePort = agent.Spec.Service.Port
	}
	if eff.Image == "" {
		eff.Image = class.Spec.Image.DefaultImage
	}
	if len(eff.Resources.Requests) == 0 && len(eff.Resources.Limits) == 0 {
		eff.Resources = class.Spec.Resources.Defaults
	}
	eff.Resources = clampResources(eff.Resources, class.Spec.Resources.MaxLimits)
	if eff.ServicePort == 0 {
		eff.ServicePort = defaultHealthPort
	}
	if agent.Spec.Persistence.ExistingClaim != nil {
		eff.ExistingClaim = *agent.Spec.Persistence.ExistingClaim
	}
	if agent.Spec.Persistence.SizeGi != nil {
		eff.PVCSizeGi = *agent.Spec.Persistence.SizeGi
	} else {
		eff.PVCSizeGi = class.Spec.Persistence.DefaultSizeGi
	}
	if max := class.Spec.Persistence.MaxSizeGi; max > 0 && eff.PVCSizeGi > max {
		eff.PVCSizeGi = max
	}
	for _, p := range agent.Spec.Providers {
		eff.Providers = append(eff.Providers, p.ProviderRef.Name)
	}

	// Lifecycle: agent values default from the class and are capped by it.
	pick := func(v, def, max time.Duration) time.Duration {
		if v == 0 {
			v = def
		}
		if max > 0 && v > max {
			v = max
		}
		return v
	}
	lc, clc := agent.Spec.Lifecycle, class.Spec.Lifecycle
	eff.IdleTimeout = pick(lc.IdleTimeout.Duration, clc.DefaultIdleTimeout.Duration, clc.MaxIdleTimeout.Duration)
	eff.HibernationDelay = pick(lc.HibernationDelay.Duration, clc.DefaultHibernationDelay.Duration, clc.MaxHibernationDelay.Duration)
	eff.HibernationEnabled = lc.HibernationEnabled
	eff.ActivitySource = lc.ActivitySource
	if eff.ActivitySource == "" {
		eff.ActivitySource = "gatewayTraffic"
	}
	return eff
}

// clampResources caps limits (and any requests above the cap) at maxLimits.
func clampResources(res corev1.ResourceRequirements, maxLimits corev1.ResourceList) corev1.ResourceRequirements {
	if len(maxLimits) == 0 {
		return res
	}
	out := corev1.ResourceRequirements{
		Requests: res.Requests.DeepCopy(),
		Limits:   res.Limits.DeepCopy(),
	}
	for name, cap := range maxLimits {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		if lim, ok := out.Limits[name]; !ok || lim.Cmp(cap) > 0 {
			out.Limits[name] = cap.DeepCopy()
		}
		if req, ok := out.Requests[name]; ok && req.Cmp(cap) > 0 {
			out.Requests[name] = cap.DeepCopy()
		}
	}
	return out
}

// imageAllowed reports whether image matches at least one path.Match glob in
// allowed. An empty list allows any image.
func imageAllowed(image string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, pattern := range allowed {
		if ok, err := path.Match(pattern, image); err == nil && ok {
			return true
		}
	}
	return false
}

// namespaceAllowed reports whether ns matches at least one glob in allowed. An
// empty list allows every namespace.
func namespaceAllowed(ns string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, pattern := range allowed {
		if ok, err := path.Match(pattern, ns); err == nil && ok {
			return true
		}
	}
	return false
}

// hashableSpec is the replacement-triggering subset of the derived Pod spec:
// image, resources, command, args, env, provider wiring. Only these fields
// participate in drift detection.
type hashableSpec struct {
	Image     string                      `json:"image"`
	Command   []string                    `json:"command,omitempty"`
	Args      []string                    `json:"args,omitempty"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources"`
	Providers []string                    `json:"providers,omitempty"`
}

// podSpecHash returns the drift-detection hash for an effective spec.
func podSpecHash(eff effectiveAgentSpec) string {
	providers := append([]string(nil), eff.Providers...)
	sort.Strings(providers)
	h := hashableSpec{
		Image:     eff.Image,
		Command:   eff.Command,
		Args:      eff.Args,
		Env:       eff.Env,
		Resources: eff.Resources,
		Providers: providers,
	}
	raw, err := json.Marshal(h)
	if err != nil {
		// hashableSpec is plain data; Marshal cannot fail on it.
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func agentCertificateName(agentName string) string { return agentName + "-tls" }
func agentPVCName(agentName string) string         { return agentName + "-memory" }
func agentServiceAccountName(agentName string) string {
	return "agent-" + agentName
}

func gatewayEndpoint(operatorNamespace string) string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", gatewayServiceName, operatorNamespace, gatewayPort)
}

// desiredCertificate builds the per-Agent cert-manager Certificate: Service DNS
// SANs, server+client auth, issued from the kaalm-ca-issuer ClusterIssuer.
// See docs/src/security/tls.md.
func desiredCertificate(agent *kaalmv1alpha1.Agent) *cmapi.Certificate {
	name, ns := agent.Name, agent.Namespace
	return &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: agentCertificateName(name), Namespace: ns},
		Spec: cmapi.CertificateSpec{
			SecretName: agentCertificateName(name),
			IssuerRef:  cmmeta.ObjectReference{Name: clusterIssuerName, Kind: "ClusterIssuer"},
			DNSNames: []string{
				fmt.Sprintf("%s.%s.svc.cluster.local", name, ns),
				fmt.Sprintf("%s.%s.svc", name, ns),
				fmt.Sprintf("%s.%s", name, ns),
			},
			Duration:    &metav1.Duration{Duration: certDuration},
			RenewBefore: &metav1.Duration{Duration: certRenewBefore},
			Usages:      []cmapi.KeyUsage{cmapi.UsageServerAuth, cmapi.UsageClientAuth},
		},
	}
}

// desiredServiceAccount is the per-Agent identity with no RoleBindings: the
// agent has no Kubernetes API access unless explicitly granted.
func desiredServiceAccount(agent *kaalmv1alpha1.Agent) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName(agent.Name), Namespace: agent.Namespace},
	}
}

// desiredService is the ClusterIP Service fronting the agent's HTTPS listener.
// targetPort is the literal health port, decoupled from the Service-facing port.
func desiredService(agent *kaalmv1alpha1.Agent, eff effectiveAgentSpec) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: agentPodLabels(agent),
			Ports: []corev1.ServicePort{{
				Name:       "https",
				Port:       eff.ServicePort,
				TargetPort: intstr.FromInt32(eff.HealthPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// desiredPVC provisions the agent's durable volume. Callers must not invoke it
// when persistence is disabled or an existingClaim is referenced.
func desiredPVC(agent *kaalmv1alpha1.Agent, class *kaalmv1alpha1.AgentClass, eff effectiveAgentSpec) *corev1.PersistentVolumeClaim {
	size := eff.PVCSizeGi
	if size <= 0 {
		size = 1
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: agentPVCName(agent.Name), Namespace: agent.Namespace},
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

func agentPodLabels(agent *kaalmv1alpha1.Agent) map[string]string {
	return map[string]string{
		"kaalm.io/agent":    agent.Name,
		"kaalm.io/workload": "agent",
	}
}

// desiredPod derives the agent Pod: injected env and probes per the runtime
// contract, the single projected TLS volume at /var/run/kaalm, and the
// drift-detection hash annotation.
func desiredPod(agent *kaalmv1alpha1.Agent, eff effectiveAgentSpec, operatorNamespace string) *corev1.Pod {
	labels := map[string]string{}
	for k, v := range eff.PodLabels {
		labels[k] = v
	}
	for k, v := range agentPodLabels(agent) {
		labels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range eff.PodAnnotations {
		annotations[k] = v
	}
	annotations[annotationPodSpecHash] = podSpecHash(eff)

	env := []corev1.EnvVar{
		{Name: "KAALM_HEALTH_PORT", Value: fmt.Sprintf("%d", eff.HealthPort)},
		{Name: "KAALM_GATEWAY_ENDPOINT", Value: gatewayEndpoint(operatorNamespace)},
		{Name: "KAALM_CA_CERT", Value: tlsMountPath + "/ca.crt"},
		{Name: "KAALM_TLS_CERT", Value: tlsMountPath + "/tls.crt"},
		{Name: "KAALM_TLS_KEY", Value: tlsMountPath + "/tls.key"},
	}
	env = append(env, eff.Env...)

	volumes := []corev1.Volume{{
		Name: "kaalm-tls",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: agentCertificateName(agent.Name)},
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
	mounts := []corev1.VolumeMount{{Name: "kaalm-tls", MountPath: tlsMountPath, ReadOnly: true}}

	if eff.PersistenceOn {
		claim := eff.ExistingClaim
		if claim == "" {
			claim = agentPVCName(agent.Name)
		}
		mountPath := eff.MountPath
		if mountPath == "" {
			mountPath = "/var/agent/memory"
		}
		volumes = append(volumes, corev1.Volume{
			Name: "agent-memory",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "agent-memory", MountPath: mountPath})
	}

	probe := func(probePath string) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   probePath,
					Port:   intstr.FromInt32(eff.HealthPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
		}
	}

	container := corev1.Container{
		Name:            agentContainer,
		Image:           eff.Image,
		Command:         eff.Command,
		Args:            eff.Args,
		Env:             env,
		Resources:       eff.Resources,
		ImagePullPolicy: eff.PullPolicy,
		SecurityContext: eff.ContainerSec,
		VolumeMounts:    mounts,
		Ports:           []corev1.ContainerPort{{Name: "https", ContainerPort: eff.HealthPort}},
		LivenessProbe:   probe("/livez"),
		ReadinessProbe:  probe("/readyz"),
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: agent.Name + "-",
			Namespace:    agent.Namespace,
			Labels:       labels,
			Annotations:  annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			ServiceAccountName:            agentServiceAccountName(agent.Name),
			RuntimeClassName:              eff.RuntimeClassName,
			ImagePullSecrets:              eff.ImagePullSecrets,
			SecurityContext:               eff.PodSecurity,
			TerminationGracePeriodSeconds: eff.TerminationGrace,
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
		},
	}
}

// desiredNetworkPolicy synthesizes the per-Agent policy from the AgentClass:
// egress to the gateway and DNS plus allowedCIDRs, ingress from the gateway on
// the health port, and optional same-namespace ingress. allowedHosts (FQDN
// rules) are deliberately not synthesized here: they require a CNI-specific
// policy kind and land in the hardening phase; when unsupported they are
// ignored and the AgentClassReconciler emits the Warning.
func desiredNetworkPolicy(
	agent *kaalmv1alpha1.Agent, class *kaalmv1alpha1.AgentClass, eff effectiveAgentSpec, operatorNamespace string,
) *networkingv1.NetworkPolicy {
	protoTCP := corev1.ProtocolTCP
	protoUDP := corev1.ProtocolUDP
	gwPort := intstr.FromInt32(gatewayPort)
	dnsPort := intstr.FromInt32(53)
	healthPort := intstr.FromInt32(eff.HealthPort)

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

	ingress := []networkingv1.NetworkPolicyIngressRule{
		{From: []networkingv1.NetworkPolicyPeer{gatewayPeer}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protoTCP, Port: &healthPort}}},
	}
	if class.Spec.Network.AllowSameNamespaceIngress {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: agentPodLabels(agent)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}
