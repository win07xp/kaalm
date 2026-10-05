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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
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

	// The projected TLS volume and its keys, shared by the Agent and AgentTask
	// desired-state builders so the two cannot drift apart.
	tlsVolumeName = "kaalm-tls"
	tlsCertKey    = "tls.crt"
	tlsKeyKey     = "tls.key"
	caCertKey     = "ca.crt"

	// The handler ConfigMap mount consumed by the reference base images.
	// Deliberately outside /var/run/kaalm, which belongs to the projected TLS
	// volume and its rotation watch (docs/src/runtime/base-images.md).
	handlerVolumeName = "kaalm-handler"
	handlerMountPath  = "/opt/kaalm/handler"

	// defaultMemoryMountPath is where spec.persistence lands when the Agent
	// names no mountPath. Both reference runtimes default their memory
	// directory to the same path, so an Agent that takes the default keeps
	// working whether or not $KAALM_MEMORY_DIR reaches it.
	defaultMemoryMountPath = "/var/agent/memory"

	// Selector keys used when synthesizing NetworkPolicy peers.
	labelKeyNamespaceName = "kubernetes.io/metadata.name"
	labelKeyComponent     = "app.kubernetes.io/component"
	labelKeyWorkload      = "kaalm.io/workload"

	schemeHTTPS = "https"

	// componentGateway is the gateway Deployment's component label value.
	componentGateway = "gateway"

	// workloadAgent is the workload label value on agent Pods. It also selects
	// the peers of the same-namespace ingress rule, so the two cannot drift.
	workloadAgent = "agent"

	// annotationPodSpecHash carries the derived-Pod-spec hash for drift
	// detection (the Deployment pod-template-hash idiom). Never compare
	// against the live Pod object: apiserver defaulting would report
	// perpetual drift.
	annotationPodSpecHash = "kaalm.io/pod-spec-hash"

	// annotationPodSpecHashVersion names the formula that produced
	// annotationPodSpecHash. A Pod without it was hashed by formula 1.
	annotationPodSpecHashVersion = "kaalm.io/pod-spec-hash-version"

	// podSpecHashVersion is the formula podSpecHash implements.
	podSpecHashVersion = "2"

	agentContainer = "agent"
)

// DefaultCertDuration and DefaultCertRenewBefore are the per-workload
// Certificate lifetime when the controller flags leave it unset (90d and 30d).
const (
	DefaultCertDuration    = 2160 * time.Hour
	DefaultCertRenewBefore = 720 * time.Hour
)

// CertLifetime is the duration and renewBefore of every per-workload
// Certificate, set from --cert-duration and --cert-renew-before. A zero field
// takes its default.
type CertLifetime struct {
	Duration    time.Duration
	RenewBefore time.Duration
}

// Validate rejects a lifetime cert-manager cannot honor, so a bad value
// fails controller startup instead of every Certificate write: duration at
// least cert-manager's one-hour minimum, renewBefore at least its
// five-minute minimum, and renewBefore shorter than duration.
func (l CertLifetime) Validate() error {
	if l.Duration < cmapi.MinimumCertificateDuration {
		return fmt.Errorf("certificate duration (%s) must be at least %s", l.Duration, cmapi.MinimumCertificateDuration)
	}
	if l.RenewBefore < cmapi.MinimumRenewBefore {
		return fmt.Errorf("certificate renewBefore (%s) must be at least %s", l.RenewBefore, cmapi.MinimumRenewBefore)
	}
	if l.RenewBefore >= l.Duration {
		return fmt.Errorf("certificate renewBefore (%s) must be shorter than duration (%s)", l.RenewBefore, l.Duration)
	}
	return nil
}

func (l CertLifetime) resolve() (duration, renewBefore time.Duration) {
	duration, renewBefore = l.Duration, l.RenewBefore
	if duration == 0 {
		duration = DefaultCertDuration
	}
	if renewBefore == 0 {
		renewBefore = DefaultCertRenewBefore
	}
	return duration, renewBefore
}

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
	AutomountToken   bool
	TerminationGrace *int64
	PodLabels        map[string]string
	PodAnnotations   map[string]string
	Providers        []string
	HandlerConfigMap string

	// Lifecycle knobs, defaulted from the class and capped by it.
	IdleTimeout        time.Duration
	HibernationDelay   time.Duration
	WakeTimeout        time.Duration
	HibernationEnabled bool
	ActivitySource     string

	// LegacyClaims is used only by podSpecHashV1, to reproduce the hash of
	// Pods created by a release that kept container claims. It never reaches
	// the Pod or podSpecHash. Remove it together with podSpecHashV1.
	LegacyClaims []corev1.ResourceClaim
}

// deriveEffectiveSpec merges class defaults into the Agent's spec and clamps
// resources to the class maximum.
func deriveEffectiveSpec(agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass) effectiveAgentSpec {
	eff := effectiveAgentSpec{
		Image:            agent.Spec.Image,
		Command:          agent.Spec.Command,
		Args:             agent.Spec.Args,
		Env:              agent.Spec.Env,
		HealthPort:       defaultHealthPort,
		ServiceEnabled:   true,
		PersistenceOn:    agent.Spec.Persistence.Enabled,
		MountPath:        agent.Spec.Persistence.MountPath,
		RuntimeClassName: class.Spec.Runtime.RuntimeClassName,
		PullPolicy:       class.Spec.Image.PullPolicy,
		ImagePullSecrets: class.Spec.Image.ImagePullSecrets,
		PodSecurity:      restrictedPodSecurity(class.Spec.Security.PodSecurityContext),
		ContainerSec:     restrictedContainerSecurity(class.Spec.Security.ContainerSecurityContext),
		AutomountToken:   class.Spec.Security.AutomountServiceAccountToken,
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
	eff.Resources, eff.LegacyClaims = effectiveResources(agent.Spec.Resources, class.Spec.Resources)
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
	if agent.Spec.Handler != nil {
		eff.HandlerConfigMap = agent.Spec.Handler.ConfigMapRef.Name
	}

	// Lifecycle: agent values default from the class and are capped by it.
	lc, clc := agent.Spec.Lifecycle, class.Spec.Lifecycle
	eff.IdleTimeout = pickDuration(lc.IdleTimeout.Duration, clc.DefaultIdleTimeout.Duration, clc.MaxIdleTimeout.Duration)
	eff.HibernationDelay = pickDuration(lc.HibernationDelay.Duration,
		clc.DefaultHibernationDelay.Duration, clc.MaxHibernationDelay.Duration)
	eff.WakeTimeout = pickDuration(lc.WakeTimeout.Duration, clc.DefaultWakeTimeout.Duration, clc.MaxWakeTimeout.Duration)
	eff.HibernationEnabled = lc.HibernationEnabled
	eff.ActivitySource = lc.ActivitySource
	if eff.ActivitySource == "" {
		eff.ActivitySource = "gatewayTraffic"
	}
	return eff
}

// pickDuration derives a class-bounded lifecycle duration (rules 8 to 10
// and 42): the workload's own value, else the class default, clamped to the
// class max. Zero means unset at every position, so a max alone supplies no
// value.
func pickDuration(v, def, max time.Duration) time.Duration {
	if v == 0 {
		v = def
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

// pickSeconds is pickDuration for an optional count of seconds, where zero
// is a real value and nil means unset (rule 43).
func pickSeconds(v, def, max *int32) *int32 {
	if v == nil {
		v = def
	}
	if v == nil {
		return nil
	}
	out := *v
	if max != nil && out > *max {
		out = *max
	}
	return &out
}

// effectiveResources resolves a workload's container resources: its own
// block, else the class defaults (a block with neither requests nor limits
// counts as unset), clamped to the class maxLimits. Claims are always
// dropped: a container claim must name an entry in pod.spec.resourceClaims,
// which the controller never sets. With Dynamic Resource Allocation enabled
// the API server rejects such a Pod; with it disabled it strips the claims.
//
// legacyClaims holds the claims a release with hash formula 1 passed to the
// container and hashed, which it did only when the class set no maxLimits.
// res is a copy, so clearing its claims never mutates the workload or class.
func effectiveResources(own corev1.ResourceRequirements, class kaalmv1beta1.AgentClassResources) (
	res corev1.ResourceRequirements, legacyClaims []corev1.ResourceClaim) {
	res = own
	if len(res.Requests) == 0 && len(res.Limits) == 0 {
		res = class.Defaults
	}
	if len(class.MaxLimits) == 0 {
		legacyClaims = res.Claims
	}
	res = clampResources(res, class.MaxLimits)
	res.Claims = nil
	return res, legacyClaims
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
// allowed. An empty list allows any image. A malformed pattern matches
// nothing; the AgentClassReconciler reports it on Ready (rule 52).
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
// empty list allows none, as the provider docs have always stated: "*" is the
// explicit opt-in to every namespace (behavior aligned in v0.4.0). A
// malformed pattern matches nothing; the provider reconcilers report it on
// Ready (rule 51).
func namespaceAllowed(ns string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, pattern := range allowed {
		if ok, err := path.Match(pattern, ns); err == nil && ok {
			return true
		}
	}
	return false
}

// hashableSpec is the replacement-triggering subset of the derived Pod spec:
// image, resources, command, args, env, provider wiring, handler-mount wiring
// (the ConfigMap name, never its content), and every Pod input the AgentClass
// controls. The class inputs are the derived values (the restricted security
// baseline already merged in), so a change to the baseline itself also rolls
// Pods. Only these fields participate in drift detection. The TLS Secret name
// is not a hash input, because it comes from the Certificate, so a Certificate
// that keeps an older name never causes drift.
type hashableSpec struct {
	Image     string                      `json:"image"`
	Command   []string                    `json:"command,omitempty"`
	Args      []string                    `json:"args,omitempty"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources"`
	Providers []string                    `json:"providers,omitempty"`
	Handler   string                      `json:"handler,omitempty"`

	RuntimeClassName *string                       `json:"runtimeClassName,omitempty"`
	PullPolicy       corev1.PullPolicy             `json:"pullPolicy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	PodSecurity      *corev1.PodSecurityContext    `json:"podSecurity,omitempty"`
	ContainerSec     *corev1.SecurityContext       `json:"containerSecurity,omitempty"`
	AutomountToken   bool                          `json:"automountToken"`
	TerminationGrace *int64                        `json:"terminationGrace,omitempty"`
	// encoding/json writes map keys in sorted order, so the maps hash
	// deterministically.
	PodLabels      map[string]string `json:"podLabels,omitempty"`
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}

// podSpecHash returns the drift-detection hash for an effective spec.
func podSpecHash(eff effectiveAgentSpec) string {
	providers := append([]string(nil), eff.Providers...)
	sort.Strings(providers)
	pullSecrets := append([]corev1.LocalObjectReference(nil), eff.ImagePullSecrets...)
	sort.Slice(pullSecrets, func(i, j int) bool { return pullSecrets[i].Name < pullSecrets[j].Name })
	h := hashableSpec{
		Image:            eff.Image,
		Command:          eff.Command,
		Args:             eff.Args,
		Env:              eff.Env,
		Resources:        eff.Resources,
		Providers:        providers,
		Handler:          eff.HandlerConfigMap,
		RuntimeClassName: eff.RuntimeClassName,
		PullPolicy:       eff.PullPolicy,
		ImagePullSecrets: pullSecrets,
		PodSecurity:      eff.PodSecurity,
		ContainerSec:     eff.ContainerSec,
		AutomountToken:   eff.AutomountToken,
		TerminationGrace: eff.TerminationGrace,
		PodLabels:        withoutKeys(eff.PodLabels, agentPodLabelKeys...),
		PodAnnotations:   withoutKeys(eff.PodAnnotations, annotationPodSpecHash, annotationPodSpecHashVersion),
	}
	raw, err := json.Marshal(h)
	if err != nil {
		// hashableSpec is plain data; Marshal cannot fail on it.
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// hashableSpecV1 and podSpecHashV1 freeze hash formula 1, which v1.0.0
// wrote on every agent Pod. They exist only to rewrite the hash of Pods created before
// the formula changed, so an upgrade does not replace them. Remove them once
// no supported upgrade path starts from a release that wrote v1 hashes.
// Do not edit: for a spec as v1.0.0 derived it, the output must stay
// byte-for-byte what v1.0.0 produced. LegacyClaims restores the container
// claims v1.0.0 kept (and hashed) when the class set no maxLimits.
type hashableSpecV1 struct {
	Image     string                      `json:"image"`
	Command   []string                    `json:"command,omitempty"`
	Args      []string                    `json:"args,omitempty"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources"`
	Providers []string                    `json:"providers,omitempty"`
	Handler   string                      `json:"handler,omitempty"`
}

func podSpecHashV1(eff effectiveAgentSpec) string {
	providers := append([]string(nil), eff.Providers...)
	sort.Strings(providers)
	res := eff.Resources
	if len(eff.LegacyClaims) > 0 {
		res.Claims = eff.LegacyClaims
	}
	h := hashableSpecV1{
		Image:     eff.Image,
		Command:   eff.Command,
		Args:      eff.Args,
		Env:       eff.Env,
		Resources: res,
		Providers: providers,
		Handler:   eff.HandlerConfigMap,
	}
	raw, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// withoutKeys copies m minus the given keys. desiredPod overwrites those keys
// with the controller's own values, so a class value for one never reaches the
// Pod and must not change the hash.
func withoutKeys(m map[string]string, drop ...string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

func agentCertificateName(agentName string) string { return agentName + "-tls" }
func agentPVCName(agentName string) string         { return agentName + "-memory" }
func agentServiceAccountName(agentName string) string {
	return "agent-" + agentName
}

// certSecretUIDChars is how many leading characters of the workload's UID
// end the output Secret name of a new per-workload Certificate.
const certSecretUIDChars = 8

// certificateSecretName is the output Secret name of a new per-workload
// Certificate: the workload name, "-tls-", and the first certSecretUIDChars
// characters of the workload's UID. The UID suffix makes the name unique to
// this workload object, so it cannot name a Secret that existed before the
// workload, and a workload re-created with the same name gets a new one. An
// existing Certificate keeps whatever spec.secretName it carries. A UID
// shorter than the suffix is used whole.
func certificateSecretName(workloadName string, uid types.UID) string {
	u := string(uid)
	if len(u) > certSecretUIDChars {
		u = u[:certSecretUIDChars]
	}
	return workloadName + "-tls-" + u
}

func gatewayEndpoint(operatorNamespace string) string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", gatewayServiceName, operatorNamespace, gatewayPort)
}

// desiredCertificate builds the per-Agent cert-manager Certificate: Service DNS
// SANs, server+client auth, issued from the kaalm-ca-issuer ClusterIssuer,
// written to the Secret certificateSecretName names. See
// docs/src/security/tls.md.
func desiredCertificate(agent *kaalmv1beta1.Agent, lifetime CertLifetime) *cmapi.Certificate {
	name, ns := agent.Name, agent.Namespace
	duration, renewBefore := lifetime.resolve()
	return &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: agentCertificateName(name), Namespace: ns},
		Spec: cmapi.CertificateSpec{
			SecretName: certificateSecretName(name, agent.UID),
			IssuerRef:  cmmeta.ObjectReference{Name: clusterIssuerName, Kind: "ClusterIssuer"},
			DNSNames: []string{
				fmt.Sprintf("%s.%s.svc.cluster.local", name, ns),
				fmt.Sprintf("%s.%s.svc", name, ns),
				fmt.Sprintf("%s.%s", name, ns),
			},
			Duration:    &metav1.Duration{Duration: duration},
			RenewBefore: &metav1.Duration{Duration: renewBefore},
			Usages:      []cmapi.KeyUsage{cmapi.UsageServerAuth, cmapi.UsageClientAuth},
		},
	}
}

// desiredServiceAccount is the per-Agent identity with no RoleBindings. The
// Pod mounts its token only when the class sets
// security.automountServiceAccountToken, so by default the agent has no
// Kubernetes API access.
func desiredServiceAccount(agent *kaalmv1beta1.Agent) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName(agent.Name), Namespace: agent.Namespace},
	}
}

// desiredService is the ClusterIP Service fronting the agent's HTTPS listener.
// targetPort is the literal health port, decoupled from the Service-facing port.
func desiredService(agent *kaalmv1beta1.Agent, eff effectiveAgentSpec) *corev1.Service {
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
func desiredPVC(agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec) *corev1.PersistentVolumeClaim {
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

// labelKeyAgent names the Agent a Pod belongs to.
const labelKeyAgent = "kaalm.io/agent"

// agentPodLabelKeys are the label keys agentPodLabels sets on every agent Pod.
var agentPodLabelKeys = []string{labelKeyAgent, labelKeyWorkload}

func agentPodLabels(agent *kaalmv1beta1.Agent) map[string]string {
	return map[string]string{
		labelKeyAgent:    agent.Name,
		labelKeyWorkload: workloadAgent,
	}
}

// tlsSecretOf returns the Secret the Pod's kaalm-tls projected volume names,
// or "" when the Pod has no such volume or the volume has no Secret source.
func tlsSecretOf(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.Name != tlsVolumeName || v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.Secret != nil {
				return src.Secret.Name
			}
		}
	}
	return ""
}

// desiredPod derives the agent Pod: injected env and probes per the runtime
// contract, the single projected TLS volume at /var/run/kaalm, and the
// drift-detection hash annotation. The TLS volume projects tlsSecret, the
// Secret the Agent's Certificate names in spec.secretName.
func desiredPod(agent *kaalmv1beta1.Agent, eff effectiveAgentSpec, operatorNamespace, tlsSecret string) *corev1.Pod {
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
	annotations[annotationPodSpecHashVersion] = podSpecHashVersion

	// Resolved once: the same path names the volume mount and the memory
	// directory the runtime is told about.
	memoryMountPath := eff.MountPath
	if memoryMountPath == "" {
		memoryMountPath = defaultMemoryMountPath
	}

	env := []corev1.EnvVar{
		{Name: "KAALM_HEALTH_PORT", Value: fmt.Sprintf("%d", eff.HealthPort)},
		{Name: "KAALM_GATEWAY_ENDPOINT", Value: gatewayEndpoint(operatorNamespace)},
		{Name: "KAALM_OPERATOR_NAMESPACE", Value: operatorNamespace},
		{Name: "KAALM_CA_CERT", Value: tlsMountPath + "/ca.crt"},
		{Name: "KAALM_TLS_CERT", Value: tlsMountPath + "/tls.crt"},
		{Name: "KAALM_TLS_KEY", Value: tlsMountPath + "/tls.key"},
	}
	// Injected iff a handler is configured: its absence is how a base image
	// knows to serve the built-in default handler (docs/src/runtime/base-images.md).
	if eff.HandlerConfigMap != "" {
		env = append(env, corev1.EnvVar{Name: "KAALM_HANDLER_PATH", Value: handlerMountPath})
	}
	// Injected iff there is a volume: it points the runtime's state file at the
	// mount, so a custom mountPath survives hibernation (contract item 7).
	// Without a volume there is nothing to name and the runtime's own default
	// applies.
	if eff.PersistenceOn {
		env = append(env, corev1.EnvVar{Name: "KAALM_MEMORY_DIR", Value: memoryMountPath})
	}
	env = append(env, eff.Env...)

	volumes := []corev1.Volume{{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: tlsSecret},
						Items: []corev1.KeyToPath{
							{Key: tlsCertKey, Path: tlsCertKey},
							{Key: tlsKeyKey, Path: tlsKeyKey},
						},
					}},
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: caBundleConfigMap},
						Items:                []corev1.KeyToPath{{Key: caCertKey, Path: caCertKey}},
					}},
				},
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true}}

	if eff.PersistenceOn {
		claim := eff.ExistingClaim
		if claim == "" {
			claim = agentPVCName(agent.Name)
		}
		volumes = append(volumes, corev1.Volume{
			Name: "agent-memory",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "agent-memory", MountPath: memoryMountPath})
	}

	if eff.HandlerConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: handlerVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: eff.HandlerConfigMap},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: handlerVolumeName, MountPath: handlerMountPath, ReadOnly: true})
	}

	if readOnlyRoot(eff.ContainerSec) {
		v, m := tmpVolume()
		volumes = append(volumes, v)
		mounts = append(mounts, m)
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
			AutomountServiceAccountToken:  &eff.AutomountToken,
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
// egress to the gateway and the DNS Pods the DNSSelector picks plus allowedCIDRs, ingress from the gateway on
// the health port, and optional ingress from the namespace's other agent Pods
// on that same port. allowedHosts (FQDN rules) need a CNI-specific policy
// kind, so desiredFQDNPolicy builds them as a separate CiliumNetworkPolicy.
func desiredNetworkPolicy(
	agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec, operatorNamespace string,
	dns DNSSelector,
) *networkingv1.NetworkPolicy {
	protoTCP := corev1.ProtocolTCP
	protoUDP := corev1.ProtocolUDP
	gwPort := intstr.FromInt32(gatewayPort)
	dnsPort := intstr.FromInt32(53)
	healthPort := intstr.FromInt32(eff.HealthPort)

	gatewayPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{labelKeyNamespaceName: operatorNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{labelKeyComponent: componentGateway},
		},
	}
	dnsPeer := dns.peer()

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
		// Scoped to agent Pods on the health port: the opt-in is for delivery
		// between agents, and the agent container is untrusted, so a Pod that
		// Kaalm does not manage never reaches it.
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelKeyWorkload: workloadAgent},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protoTCP, Port: &healthPort}},
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
