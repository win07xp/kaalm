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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestPodSpecHash_StableAndSensitive(t *testing.T) {
	base := effectiveAgentSpec{
		Image:     "registry/agents/a:v1",
		Env:       []corev1.EnvVar{{Name: "X", Value: "1"}},
		Providers: []string{"b", "a"},
	}
	h1 := podSpecHash(base)
	if h2 := podSpecHash(base); h2 != h1 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	// Provider order must not matter.
	reordered := base
	reordered.Providers = []string{"a", "b"}
	if h := podSpecHash(reordered); h != h1 {
		t.Fatalf("hash sensitive to provider order: %s vs %s", h, h1)
	}
	// A replacement-triggering change must change the hash.
	changed := base
	changed.Image = "registry/agents/a:v2"
	if h := podSpecHash(changed); h == h1 {
		t.Fatal("hash unchanged after image change")
	}
	changed = base
	changed.Env = []corev1.EnvVar{{Name: "X", Value: "2"}}
	if h := podSpecHash(changed); h == h1 {
		t.Fatal("hash unchanged after env change")
	}
	// A non-replacement field (service port) must not change the hash.
	cosmetic := base
	cosmetic.ServicePort = 9090
	if h := podSpecHash(cosmetic); h != h1 {
		t.Fatal("hash sensitive to non-replacement field")
	}
}

func TestClampResources(t *testing.T) {
	maxLimits := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("2Gi"),
	}
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	got := clampResources(res, maxLimits)
	if cpu := got.Limits[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("cpu limit not clamped: %s", cpu.String())
	}
	if mem := got.Limits[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("memory limit below cap was altered: %s", mem.String())
	}
	if cpu := got.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("cpu request above cap not clamped: %s", cpu.String())
	}
	// A limit absent from the workload spec is filled in at the cap.
	empty := clampResources(corev1.ResourceRequirements{}, maxLimits)
	if mem := empty.Limits[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("missing limit not defaulted to cap: %s", mem.String())
	}
}

func TestImageAllowed(t *testing.T) {
	allowed := []string{"registry.internal/agents/*", "docker.io/library/nginx"}
	cases := []struct {
		image string
		want  bool
	}{
		{"registry.internal/agents/support:v1", true},
		{"docker.io/library/nginx", true},
		{"evil.io/agents/support:v1", false},
		{"registry.internal/other/support:v1", false},
	}
	for _, c := range cases {
		if got := imageAllowed(c.image, allowed); got != c.want {
			t.Errorf("imageAllowed(%q) = %v, want %v", c.image, got, c.want)
		}
	}
	if !imageAllowed("anything", nil) {
		t.Error("empty allowlist must allow any image")
	}
}

func TestNamespaceAllowed(t *testing.T) {
	if !namespaceAllowed("team-support", []string{"team-*"}) {
		t.Error("glob should match")
	}
	if namespaceAllowed("prod", []string{"team-*"}) {
		t.Error("glob should not match")
	}
	if !namespaceAllowed("anything", nil) {
		t.Error("empty list allows all")
	}
}

func TestDeriveEffectiveSpec_ClassDefaults(t *testing.T) {
	class := &agentryv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: agentryv1alpha1.AgentClassSpec{
			Image: agentryv1alpha1.AgentClassImage{DefaultImage: "registry/default:v1"},
			Resources: agentryv1alpha1.AgentClassResources{
				Defaults: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				},
			},
			Persistence: agentryv1alpha1.AgentClassPersistence{DefaultSizeGi: 5, MaxSizeGi: 10},
		},
	}
	agent := &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec: agentryv1alpha1.AgentSpec{
			AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "standard"},
			Persistence:   agentryv1alpha1.AgentPersistence{Enabled: true},
		},
	}
	eff := deriveEffectiveSpec(agent, class)
	if eff.Image != "registry/default:v1" {
		t.Errorf("class default image not applied: %q", eff.Image)
	}
	if cpu := eff.Resources.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("250m")) != 0 {
		t.Errorf("class default resources not applied")
	}
	if eff.PVCSizeGi != 5 {
		t.Errorf("class default size not applied: %d", eff.PVCSizeGi)
	}
	if eff.HealthPort != 8080 || eff.ServicePort != 8080 {
		t.Errorf("default ports wrong: health=%d service=%d", eff.HealthPort, eff.ServicePort)
	}

	// Agent overrides win and the size cap clamps.
	size := int32(50)
	agent.Spec.Image = "registry/custom:v2"
	agent.Spec.Persistence.SizeGi = &size
	eff = deriveEffectiveSpec(agent, class)
	if eff.Image != "registry/custom:v2" {
		t.Errorf("agent image override lost: %q", eff.Image)
	}
	if eff.PVCSizeGi != 10 {
		t.Errorf("size not clamped to class max: %d", eff.PVCSizeGi)
	}
}

func TestDesiredPod_ContractInjection(t *testing.T) {
	agent := &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentSpec{
			Env: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
		},
	}
	eff := effectiveAgentSpec{Image: "img:v1", HealthPort: 8080, Env: agent.Spec.Env, PersistenceOn: true}
	pod := desiredPod(agent, eff, "agentry-system")

	envMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AGENTRY_GATEWAY_ENDPOINT"] != "https://agentry-gateway.agentry-system.svc.cluster.local:8443" {
		t.Errorf("gateway endpoint wrong: %q", envMap["AGENTRY_GATEWAY_ENDPOINT"])
	}
	if envMap["AGENTRY_CA_CERT"] != "/var/run/agentry/ca.crt" ||
		envMap["AGENTRY_TLS_CERT"] != "/var/run/agentry/tls.crt" ||
		envMap["AGENTRY_TLS_KEY"] != "/var/run/agentry/tls.key" {
		t.Errorf("TLS paths wrong: %v", envMap)
	}
	if envMap["LOG_LEVEL"] != "info" {
		t.Error("user env not merged")
	}
	c := pod.Spec.Containers[0]
	if c.LivenessProbe.HTTPGet.Path != "/livez" || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Error("probe paths wrong")
	}
	if c.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Error("probes must be HTTPS")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Error("agent pods must have restartPolicy Always")
	}
	if pod.Annotations[annotationPodSpecHash] == "" {
		t.Error("pod-spec hash annotation missing")
	}
	if pod.Spec.ServiceAccountName != "agent-sup" {
		t.Errorf("SA name wrong: %q", pod.Spec.ServiceAccountName)
	}
	// PVC volume present with the provisioned claim name.
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "sup-memory" {
			found = true
		}
	}
	if !found {
		t.Error("persistence volume missing")
	}
}

func TestDesiredCertificate_Shape(t *testing.T) {
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	cert := desiredCertificate(agent)
	if cert.Name != "sup-tls" || cert.Spec.SecretName != "sup-tls" {
		t.Errorf("cert naming wrong: %s / %s", cert.Name, cert.Spec.SecretName)
	}
	wantSANs := []string{"sup.team-a.svc.cluster.local", "sup.team-a.svc", "sup.team-a"}
	if len(cert.Spec.DNSNames) != 3 {
		t.Fatalf("want 3 SANs, got %v", cert.Spec.DNSNames)
	}
	for i, s := range wantSANs {
		if cert.Spec.DNSNames[i] != s {
			t.Errorf("SAN %d = %q, want %q", i, cert.Spec.DNSNames[i], s)
		}
	}
	if cert.Spec.IssuerRef.Name != "agentry-ca-issuer" || cert.Spec.IssuerRef.Kind != "ClusterIssuer" {
		t.Errorf("issuerRef wrong: %+v", cert.Spec.IssuerRef)
	}
	if len(cert.Spec.Usages) != 2 {
		t.Errorf("want server auth + client auth, got %v", cert.Spec.Usages)
	}
}

func TestDesiredNetworkPolicy_Rules(t *testing.T) {
	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	class := &agentryv1alpha1.AgentClass{
		Spec: agentryv1alpha1.AgentClassSpec{
			Network: agentryv1alpha1.AgentClassNetwork{
				Egress:                    agentryv1alpha1.AgentClassEgress{AllowedCIDRs: []string{"10.0.0.0/8"}},
				AllowSameNamespaceIngress: true,
			},
		},
	}
	np := desiredNetworkPolicy(agent, class, effectiveAgentSpec{HealthPort: 8080}, "agentry-system")
	// gateway egress + DNS + one CIDR
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("want 3 egress rules, got %d", len(np.Spec.Egress))
	}
	if np.Spec.Egress[2].To[0].IPBlock.CIDR != "10.0.0.0/8" {
		t.Errorf("CIDR rule wrong: %+v", np.Spec.Egress[2])
	}
	// gateway ingress + same-namespace
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("want 2 ingress rules, got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Error("policy must declare both Ingress and Egress")
	}
}
