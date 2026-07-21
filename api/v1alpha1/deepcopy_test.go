package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ptr returns a pointer to a copy of v, for building fully-populated fixtures.
func ptr[T any](v T) *T { return &v }

// mutatedStr is the sentinel value the mutate closures below write into
// copied fixtures, to prove the write never reaches the original.
const mutatedStr = "mutated"

// checkDeepCopy exercises the DeepCopy contract for a single fully-populated
// value of type T: the copy must be a distinct pointer, deeply equal to the
// original, and mutating the copy (via mutate) must never affect the
// original.
func checkDeepCopy[T any](t *testing.T, name string, original *T, deepCopy func(*T) *T, mutate func(*T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		cp := deepCopy(original)
		if cp == original {
			t.Fatalf("%s: DeepCopy returned the same pointer as the original", name)
		}
		if !reflect.DeepEqual(original, cp) {
			t.Fatalf("%s: DeepCopy() not deeply equal to original\ngot:  %#v\nwant: %#v", name, cp, original)
		}

		snapshot := deepCopy(original)
		mutate(cp)
		if reflect.DeepEqual(cp, snapshot) {
			t.Fatalf("%s: mutate closure did not actually change the copy", name)
		}
		if !reflect.DeepEqual(original, snapshot) {
			t.Fatalf("%s: mutating the copy also mutated the original (shared backing store)\noriginal: %#v\nsnapshot: %#v", name, original, snapshot)
		}
	})
}

// checkNilDeepCopy verifies that calling DeepCopy on a nil receiver returns
// nil rather than panicking.
func checkNilDeepCopy[T any](t *testing.T, name string, deepCopy func(*T) *T) {
	t.Helper()
	t.Run(name+"/nil", func(t *testing.T) {
		var nilObj *T
		if got := deepCopy(nilObj); got != nil {
			t.Fatalf("%s: DeepCopy on nil receiver = %#v, want nil", name, got)
		}
	})
}

// checkNilDeepCopyObject verifies that calling DeepCopyObject on a nil
// receiver returns a nil runtime.Object rather than panicking.
func checkNilDeepCopyObject[T any](t *testing.T, name string, deepCopyObject func(*T) runtime.Object) {
	t.Helper()
	t.Run(name+"/nilObject", func(t *testing.T) {
		var nilObj *T
		if got := deepCopyObject(nilObj); got != nil {
			t.Fatalf("%s: DeepCopyObject on nil receiver = %#v, want nil", name, got)
		}
	})
}

// ---------------------------------------------------------------------------
// Fixtures: fully-populated instances so every optional/slice/map/pointer
// branch in DeepCopyInto executes.
// ---------------------------------------------------------------------------

func fullObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   "default",
		Labels:      map[string]string{"k": "v"},
		Annotations: map[string]string{"a": "b"},
		Finalizers:  []string{"agentry.io/finalizer"},
	}
}

func fullConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "AllGood",
			Message:            "all good",
			LastTransitionTime: metav1.NewTime(time.Unix(100, 0)),
			ObservedGeneration: 1,
		},
	}
}

func newFullAgent() *Agent {
	return &Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "agentry.io/v1alpha1"},
		ObjectMeta: fullObjectMeta("agent-1"),
		Spec: AgentSpec{
			AgentClassRef: LocalObjectReference{Name: "class-a"},
			Image:         "example/agent:latest",
			Command:       []string{"/bin/agent"},
			Args:          []string{"--flag", "value"},
			Env: []corev1.EnvVar{
				{Name: "FOO", Value: "bar"},
			},
			Providers: []AgentProviderReference{
				{ProviderRef: LocalObjectReference{Name: "provider-a"}},
			},
			Resources: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			Persistence: AgentPersistence{
				Enabled:       true,
				SizeGi:        ptr(int32(10)),
				MountPath:     "/data",
				ExistingClaim: ptr("existing-pvc"),
			},
			Lifecycle: AgentLifecycle{
				IdleTimeout:        metav1.Duration{Duration: 5 * time.Minute},
				HibernationEnabled: true,
				HibernationDelay:   metav1.Duration{Duration: 10 * time.Minute},
				ActivitySource:     "both",
				WakeTimeout:        metav1.Duration{Duration: 2 * time.Minute},
			},
			Service: &AgentService{Enabled: true, Port: 8080},
			MCPServers: []AgentMCPServer{
				{Name: "mcp1", URL: "https://mcp.example.com"},
			},
		},
		Status: AgentStatus{
			ObservedGeneration:  5,
			Phase:               AgentRunning,
			Conditions:          fullConditions(),
			Endpoint:            "https://agent-1.default.svc.cluster.local",
			PodName:             "agent-1-pod",
			PVCName:             "agent-1-pvc",
			LastActivityTime:    ptr(metav1.NewTime(time.Unix(200, 0))),
			PhaseTransitionTime: ptr(metav1.NewTime(time.Unix(300, 0))),
			HibernatedAt:        ptr(metav1.NewTime(time.Unix(400, 0))),
			PreDegradedPhase:    AgentIdle,
		},
	}
}

func mutateAgent(a *Agent) {
	a.Labels["k"] = mutatedStr
	a.Spec.Command[0] = mutatedStr
	a.Spec.Args = append(a.Spec.Args, "extra")
	a.Spec.Env[0].Value = mutatedStr
	a.Spec.Providers[0].ProviderRef.Name = mutatedStr
	*a.Spec.Persistence.SizeGi = 999
	*a.Spec.Persistence.ExistingClaim = mutatedStr
	a.Spec.Service.Port = 9999
	a.Spec.MCPServers[0].Name = mutatedStr
	a.Status.Conditions[0].Message = mutatedStr
	*a.Status.LastActivityTime = metav1.NewTime(time.Unix(999, 0))
}

func TestAgentDeepCopy(t *testing.T) {
	checkDeepCopy(t, "Agent", newFullAgent(), (*Agent).DeepCopy, mutateAgent)

	original := newFullAgent()
	obj := original.DeepCopyObject()
	got, ok := obj.(*Agent)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *Agent", obj)
	}
	if got == original {
		t.Fatalf("DeepCopyObject returned the same pointer as the original")
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("DeepCopyObject() not deeply equal to original")
	}
}

func TestAgentListDeepCopy(t *testing.T) {
	list := &AgentList{
		TypeMeta: metav1.TypeMeta{Kind: "AgentList", APIVersion: "agentry.io/v1alpha1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    []Agent{*newFullAgent(), *newFullAgent()},
	}
	checkDeepCopy(t, "AgentList", list, (*AgentList).DeepCopy, func(l *AgentList) {
		l.Items[0].Name = mutatedStr
		l.Items = append(l.Items, Agent{})
	})

	obj := list.DeepCopyObject()
	got, ok := obj.(*AgentList)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentList", obj)
	}
	if !reflect.DeepEqual(list, got) {
		t.Fatalf("AgentList DeepCopyObject() not deeply equal to original")
	}
}

func newFullAgentChannel() *AgentChannel {
	return &AgentChannel{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentChannel", APIVersion: "agentry.io/v1alpha1"},
		ObjectMeta: fullObjectMeta("channel-1"),
		Spec: AgentChannelSpec{
			AgentRef: LocalObjectReference{Name: "agent-1"},
			Type:     "webhook",
			Webhook: AgentChannelWebhook{
				Path: "/channels/default/hook",
				Auth: ChannelAuth{
					Type:      "bearer",
					SecretRef: &SecretKeyReference{Name: "auth-secret", Key: "token"},
					HMAC: &ChannelHMAC{
						Header:          "X-Signature",
						Algorithm:       "sha256",
						SecretRef:       SecretKeyReference{Name: "hmac-secret", Key: "key"},
						SignaturePrefix: ptr("sha256="),
						Encoding:        "hex",
					},
				},
				UserID: ChannelExtractor{
					FromHeader: ptr("X-User-Id"),
					FromBody:   ptr("user.id"),
					Fallback:   ptr("anonymous"),
				},
				Content: ChannelExtractor{
					FromHeader: ptr("X-Body"),
					FromBody:   ptr("message.text"),
					Fallback:   ptr(""),
				},
				ResponseMode:             "async",
				CallbackURL:              ptr("https://callback.example.com/hook"),
				CallbackAuth:             &ChannelAuth{Type: "hmac", HMAC: &ChannelHMAC{Header: "X-Cb-Sig", SecretRef: SecretKeyReference{Name: "cb-secret", Key: "key"}}},
				MaxPendingAsyncResponses: 50,
			},
			Session: AgentChannelSession{Enabled: true},
		},
		Status: AgentChannelStatus{
			ObservedGeneration: 3,
			Phase:              ChannelActive,
			Conditions:         fullConditions(),
		},
	}
}

func mutateAgentChannel(c *AgentChannel) {
	c.Labels["k"] = mutatedStr
	*c.Spec.Webhook.Auth.SecretRef = SecretKeyReference{Name: mutatedStr, Key: mutatedStr}
	c.Spec.Webhook.Auth.HMAC.Header = mutatedStr
	*c.Spec.Webhook.UserID.FromHeader = mutatedStr
	*c.Spec.Webhook.CallbackURL = "https://mutated.example.com"
	c.Spec.Webhook.CallbackAuth.Type = mutatedStr
	c.Status.Conditions[0].Message = mutatedStr
}

func TestAgentChannelDeepCopy(t *testing.T) {
	checkDeepCopy(t, "AgentChannel", newFullAgentChannel(), (*AgentChannel).DeepCopy, mutateAgentChannel)

	original := newFullAgentChannel()
	obj := original.DeepCopyObject()
	got, ok := obj.(*AgentChannel)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentChannel", obj)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("AgentChannel DeepCopyObject() not deeply equal to original")
	}
}

func TestAgentChannelListDeepCopy(t *testing.T) {
	list := &AgentChannelList{
		TypeMeta: metav1.TypeMeta{Kind: "AgentChannelList", APIVersion: "agentry.io/v1alpha1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    []AgentChannel{*newFullAgentChannel(), *newFullAgentChannel()},
	}
	checkDeepCopy(t, "AgentChannelList", list, (*AgentChannelList).DeepCopy, func(l *AgentChannelList) {
		l.Items[0].Name = mutatedStr
		l.Items = append(l.Items, AgentChannel{})
	})

	obj := list.DeepCopyObject()
	if _, ok := obj.(*AgentChannelList); !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentChannelList", obj)
	}
}

func newFullAgentClass() *AgentClass {
	return &AgentClass{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentClass", APIVersion: "agentry.io/v1alpha1"},
		ObjectMeta: fullObjectMeta("class-1"),
		Spec: AgentClassSpec{
			Runtime: AgentClassRuntime{
				Backend:          "pod",
				RuntimeClassName: ptr("gvisor"),
			},
			Image: AgentClassImage{
				AllowedImages:    []string{"repo/*"},
				DefaultImage:     "repo/default:latest",
				PullPolicy:       corev1.PullIfNotPresent,
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
			},
			Resources: AgentClassResources{
				Defaults: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
				MaxLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
			Persistence: AgentClassPersistence{
				Enabled:          true,
				DefaultSizeGi:    5,
				MaxSizeGi:        20,
				StorageClassName: ptr("standard"),
				PVCRetention:     "Retain",
			},
			AllowedProviders: []LocalObjectReference{{Name: "provider-a"}},
			Network: AgentClassNetwork{
				Egress: AgentClassEgress{
					AllowedCIDRs: []string{"10.0.0.0/8"},
					AllowedHosts: []string{"api.example.com"},
				},
				AllowHostNetwork:          true,
				AllowSameNamespaceIngress: true,
			},
			Security: AgentClassSecurity{
				PodSecurityContext:       &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)},
				ContainerSecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr(true)},
			},
			Lifecycle: AgentClassLifecycle{
				DefaultIdleTimeout:            metav1.Duration{Duration: time.Minute},
				MaxIdleTimeout:                metav1.Duration{Duration: time.Hour},
				HibernationAllowed:            true,
				DefaultHibernationDelay:       metav1.Duration{Duration: 2 * time.Minute},
				MaxHibernationDelay:           metav1.Duration{Duration: 2 * time.Hour},
				DefaultWakeTimeout:            metav1.Duration{Duration: 3 * time.Minute},
				MaxWakeTimeout:                metav1.Duration{Duration: 3 * time.Hour},
				TerminationGracePeriodSeconds: ptr(int64(30)),
			},
			PodMetadata: AgentClassPodMetadata{
				Labels:      map[string]string{"team": "x"},
				Annotations: map[string]string{"note": "y"},
			},
		},
		Status: AgentClassStatus{
			ObservedGeneration: 2,
			Conditions:         fullConditions(),
			AgentsInUse:        3,
			TasksInUse:         1,
		},
	}
}

func mutateAgentClass(c *AgentClass) {
	c.Labels["k"] = mutatedStr
	*c.Spec.Runtime.RuntimeClassName = mutatedStr
	c.Spec.Image.AllowedImages[0] = mutatedStr
	c.Spec.Image.ImagePullSecrets[0].Name = mutatedStr
	c.Spec.Resources.MaxLimits[corev1.ResourceCPU] = resource.MustParse("99")
	*c.Spec.Persistence.StorageClassName = mutatedStr
	c.Spec.AllowedProviders[0].Name = mutatedStr
	c.Spec.Network.Egress.AllowedCIDRs[0] = mutatedStr
	*c.Spec.Security.PodSecurityContext.RunAsNonRoot = false
	*c.Spec.Lifecycle.TerminationGracePeriodSeconds = 0
	c.Spec.PodMetadata.Labels["team"] = mutatedStr
	c.Status.Conditions[0].Message = mutatedStr
}

func TestAgentClassDeepCopy(t *testing.T) {
	checkDeepCopy(t, "AgentClass", newFullAgentClass(), (*AgentClass).DeepCopy, mutateAgentClass)

	original := newFullAgentClass()
	obj := original.DeepCopyObject()
	got, ok := obj.(*AgentClass)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentClass", obj)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("AgentClass DeepCopyObject() not deeply equal to original")
	}
}

func TestAgentClassListDeepCopy(t *testing.T) {
	list := &AgentClassList{
		TypeMeta: metav1.TypeMeta{Kind: "AgentClassList", APIVersion: "agentry.io/v1alpha1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    []AgentClass{*newFullAgentClass(), *newFullAgentClass()},
	}
	checkDeepCopy(t, "AgentClassList", list, (*AgentClassList).DeepCopy, func(l *AgentClassList) {
		l.Items[0].Name = mutatedStr
		l.Items = append(l.Items, AgentClass{})
	})

	obj := list.DeepCopyObject()
	if _, ok := obj.(*AgentClassList); !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentClassList", obj)
	}
}

func newFullAgentTask() *AgentTask {
	return &AgentTask{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentTask", APIVersion: "agentry.io/v1alpha1"},
		ObjectMeta: fullObjectMeta("task-1"),
		Spec: AgentTaskSpec{
			AgentClassRef: LocalObjectReference{Name: "class-a"},
			Image:         "example/task:latest",
			Env: []corev1.EnvVar{
				{Name: "FOO", Value: "bar"},
			},
			Providers: []AgentProviderReference{
				{ProviderRef: LocalObjectReference{Name: "provider-a"}},
			},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			},
			Persistence: AgentTaskPersistence{
				Enabled:   true,
				SizeGi:    ptr(int32(5)),
				MountPath: "/data",
			},
			Completion: AgentTaskCompletion{
				Condition:    "agentReported",
				Timeout:      metav1.Duration{Duration: time.Hour},
				OnTimeout:    "Fail",
				BackoffLimit: 3,
			},
			Artifacts: []AgentTaskArtifact{
				{Name: "output-1"},
			},
			TTLSecondsAfterFinished: ptr(int32(3600)),
		},
		Status: AgentTaskStatus{
			ObservedGeneration:   4,
			Phase:                TaskRunning,
			Conditions:           fullConditions(),
			StartTime:            ptr(metav1.NewTime(time.Unix(500, 0))),
			CompletionTime:       ptr(metav1.NewTime(time.Unix(600, 0))),
			PodName:              "task-1-pod",
			CurrentPodUID:        "uid-1",
			Retries:              1,
			ArtifactValues:       map[string]string{"output-1": "value-1"},
			AgentReportedStatus:  "success",
			AgentReportedMessage: "done",
		},
	}
}

func mutateAgentTask(a *AgentTask) {
	a.Labels["k"] = mutatedStr
	a.Spec.Env[0].Value = mutatedStr
	a.Spec.Providers[0].ProviderRef.Name = mutatedStr
	*a.Spec.Persistence.SizeGi = 999
	a.Spec.Artifacts[0].Name = mutatedStr
	*a.Spec.TTLSecondsAfterFinished = 1
	a.Status.Conditions[0].Message = mutatedStr
	*a.Status.StartTime = metav1.NewTime(time.Unix(999, 0))
	a.Status.ArtifactValues["output-1"] = mutatedStr
}

func TestAgentTaskDeepCopy(t *testing.T) {
	checkDeepCopy(t, "AgentTask", newFullAgentTask(), (*AgentTask).DeepCopy, mutateAgentTask)

	original := newFullAgentTask()
	obj := original.DeepCopyObject()
	got, ok := obj.(*AgentTask)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentTask", obj)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("AgentTask DeepCopyObject() not deeply equal to original")
	}
}

func TestAgentTaskListDeepCopy(t *testing.T) {
	list := &AgentTaskList{
		TypeMeta: metav1.TypeMeta{Kind: "AgentTaskList", APIVersion: "agentry.io/v1alpha1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    []AgentTask{*newFullAgentTask(), *newFullAgentTask()},
	}
	checkDeepCopy(t, "AgentTaskList", list, (*AgentTaskList).DeepCopy, func(l *AgentTaskList) {
		l.Items[0].Name = mutatedStr
		l.Items = append(l.Items, AgentTask{})
	})

	obj := list.DeepCopyObject()
	if _, ok := obj.(*AgentTaskList); !ok {
		t.Fatalf("DeepCopyObject returned %T, want *AgentTaskList", obj)
	}
}

func newFullModelProvider() *ModelProvider {
	return &ModelProvider{
		TypeMeta:   metav1.TypeMeta{Kind: "ModelProvider", APIVersion: "agentry.io/v1alpha1"},
		ObjectMeta: fullObjectMeta("provider-1"),
		Spec: ModelProviderSpec{
			Type:           "anthropic",
			Endpoint:       "https://api.anthropic.com",
			CredentialsRef: SecretKeyReference{Name: "anthropic-creds", Key: "api-key"},
			Models: []ModelProviderModel{
				{ID: "claude-1", DisplayName: "Claude 1", CostPer1MInputTokens: "3.00", CostPer1MOutputTokens: "15.00"},
			},
			AllowedNamespaces: []string{"*"},
			Budget: ModelProviderBudget{
				Period:          "monthly",
				PerNamespaceUSD: "100.00",
				Policies: []ModelProviderBudgetPolicy{
					{AtPercent: 80, Action: "warn"},
					{AtPercent: 100, Action: "degrade", DegradeTo: ptr("cheap-model")},
				},
				ClusterUSD: ptr("1000.00"),
			},
			RateLimits: ModelProviderRateLimits{RequestsPerMinute: 100, TokensPerMinute: 10000},
			Fallback:   []LocalObjectReference{{Name: "fallback-provider"}},
			HealthCheck: &ModelProviderHealthCheck{
				Enabled:         true,
				IntervalSeconds: 30,
				TimeoutSeconds:  5,
			},
		},
		Status: ModelProviderStatus{
			ObservedGeneration: 6,
			Conditions:         fullConditions(),
			BudgetUsage: []ModelProviderBudgetUsage{
				{Namespace: "default", Period: "monthly", SpentUSD: "50.00", PercentUsed: 50, State: "Normal"},
			},
			ClusterSpentUSD: "500.00",
		},
	}
}

func mutateModelProvider(m *ModelProvider) {
	m.Labels["k"] = mutatedStr
	m.Spec.Models[0].DisplayName = mutatedStr
	m.Spec.AllowedNamespaces[0] = mutatedStr
	m.Spec.Budget.Policies[0].Action = mutatedStr
	*m.Spec.Budget.Policies[1].DegradeTo = mutatedStr
	*m.Spec.Budget.ClusterUSD = mutatedStr
	m.Spec.Fallback[0].Name = mutatedStr
	m.Spec.HealthCheck.IntervalSeconds = 999
	m.Status.Conditions[0].Message = mutatedStr
	m.Status.BudgetUsage[0].SpentUSD = mutatedStr
}

func TestModelProviderDeepCopy(t *testing.T) {
	checkDeepCopy(t, "ModelProvider", newFullModelProvider(), (*ModelProvider).DeepCopy, mutateModelProvider)

	original := newFullModelProvider()
	obj := original.DeepCopyObject()
	got, ok := obj.(*ModelProvider)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *ModelProvider", obj)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("ModelProvider DeepCopyObject() not deeply equal to original")
	}
}

func TestModelProviderListDeepCopy(t *testing.T) {
	list := &ModelProviderList{
		TypeMeta: metav1.TypeMeta{Kind: "ModelProviderList", APIVersion: "agentry.io/v1alpha1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    []ModelProvider{*newFullModelProvider(), *newFullModelProvider()},
	}
	checkDeepCopy(t, "ModelProviderList", list, (*ModelProviderList).DeepCopy, func(l *ModelProviderList) {
		l.Items[0].Name = mutatedStr
		l.Items = append(l.Items, ModelProvider{})
	})

	obj := list.DeepCopyObject()
	if _, ok := obj.(*ModelProviderList); !ok {
		t.Fatalf("DeepCopyObject returned %T, want *ModelProviderList", obj)
	}
}

// ---------------------------------------------------------------------------
// Nested (non-leaf) types are exercised above only through a parent's
// DeepCopyInto (e.g. `in.Persistence.DeepCopyInto(&out.Persistence)`), which
// never calls the nested type's own top-level DeepCopy() constructor. Call
// each of those directly too, so both the nil-guard and the
// new+DeepCopyInto+return path of their own DeepCopy() are covered.
// ---------------------------------------------------------------------------

func TestNestedTypesDirectDeepCopy(t *testing.T) {
	agentSpec := newFullAgent().Spec
	checkDeepCopy(t, "AgentSpec/direct", &agentSpec, (*AgentSpec).DeepCopy, func(s *AgentSpec) {
		s.Command[0] = mutatedStr
		s.Args = append(s.Args, "extra")
		s.Env[0].Value = mutatedStr
		s.Providers[0].ProviderRef.Name = mutatedStr
		*s.Persistence.SizeGi = 999
		*s.Persistence.ExistingClaim = mutatedStr
		s.Service.Port = 9999
		s.MCPServers[0].Name = mutatedStr
	})

	agentStatus := newFullAgent().Status
	checkDeepCopy(t, "AgentStatus/direct", &agentStatus, (*AgentStatus).DeepCopy, func(s *AgentStatus) {
		s.Conditions[0].Message = mutatedStr
		*s.LastActivityTime = metav1.NewTime(time.Unix(999, 0))
		*s.PhaseTransitionTime = metav1.NewTime(time.Unix(999, 0))
		*s.HibernatedAt = metav1.NewTime(time.Unix(999, 0))
	})

	agentPersistence := newFullAgent().Spec.Persistence
	checkDeepCopy(t, "AgentPersistence/direct", &agentPersistence, (*AgentPersistence).DeepCopy, func(p *AgentPersistence) {
		*p.SizeGi = 1
		*p.ExistingClaim = mutatedStr
	})

	channelSpec := newFullAgentChannel().Spec
	checkDeepCopy(t, "AgentChannelSpec/direct", &channelSpec, (*AgentChannelSpec).DeepCopy, func(s *AgentChannelSpec) {
		s.Webhook.Path = mutatedStr
		*s.Webhook.CallbackURL = mutatedStr
	})

	channelStatus := newFullAgentChannel().Status
	checkDeepCopy(t, "AgentChannelStatus/direct", &channelStatus, (*AgentChannelStatus).DeepCopy, func(s *AgentChannelStatus) {
		s.Conditions[0].Message = mutatedStr
	})

	webhook := newFullAgentChannel().Spec.Webhook
	checkDeepCopy(t, "AgentChannelWebhook/direct", &webhook, (*AgentChannelWebhook).DeepCopy, func(w *AgentChannelWebhook) {
		*w.CallbackURL = mutatedStr
		w.CallbackAuth.Type = mutatedStr
		*w.UserID.FromHeader = mutatedStr
	})

	auth := newFullAgentChannel().Spec.Webhook.Auth
	checkDeepCopy(t, "ChannelAuth/direct", &auth, (*ChannelAuth).DeepCopy, func(a *ChannelAuth) {
		*a.SecretRef = SecretKeyReference{Name: mutatedStr}
		a.HMAC.Header = mutatedStr
	})

	extractor := newFullAgentChannel().Spec.Webhook.UserID
	checkDeepCopy(t, "ChannelExtractor/direct", &extractor, (*ChannelExtractor).DeepCopy, func(e *ChannelExtractor) {
		*e.FromHeader = mutatedStr
		*e.FromBody = mutatedStr
		*e.Fallback = mutatedStr
	})

	hmacFixture := *newFullAgentChannel().Spec.Webhook.Auth.HMAC
	checkDeepCopy(t, "ChannelHMAC/direct", &hmacFixture, (*ChannelHMAC).DeepCopy, func(h *ChannelHMAC) {
		h.Header = mutatedStr
		*h.SignaturePrefix = mutatedStr
	})

	classSpec := newFullAgentClass().Spec
	checkDeepCopy(t, "AgentClassSpec/direct", &classSpec, (*AgentClassSpec).DeepCopy, func(s *AgentClassSpec) {
		*s.Runtime.RuntimeClassName = mutatedStr
		s.Image.AllowedImages[0] = mutatedStr
		*s.Persistence.StorageClassName = mutatedStr
		s.AllowedProviders[0].Name = mutatedStr
		s.Network.Egress.AllowedCIDRs[0] = mutatedStr
		*s.Security.PodSecurityContext.RunAsNonRoot = false
		*s.Lifecycle.TerminationGracePeriodSeconds = 0
		s.PodMetadata.Labels["team"] = mutatedStr
	})

	classStatus := newFullAgentClass().Status
	checkDeepCopy(t, "AgentClassStatus/direct", &classStatus, (*AgentClassStatus).DeepCopy, func(s *AgentClassStatus) {
		s.Conditions[0].Message = mutatedStr
	})

	classRuntime := newFullAgentClass().Spec.Runtime
	checkDeepCopy(t, "AgentClassRuntime/direct", &classRuntime, (*AgentClassRuntime).DeepCopy, func(r *AgentClassRuntime) {
		*r.RuntimeClassName = mutatedStr
	})

	classImage := newFullAgentClass().Spec.Image
	checkDeepCopy(t, "AgentClassImage/direct", &classImage, (*AgentClassImage).DeepCopy, func(i *AgentClassImage) {
		i.AllowedImages[0] = mutatedStr
		i.ImagePullSecrets[0].Name = mutatedStr
	})

	classResources := newFullAgentClass().Spec.Resources
	checkDeepCopy(t, "AgentClassResources/direct", &classResources, (*AgentClassResources).DeepCopy, func(r *AgentClassResources) {
		r.MaxLimits[corev1.ResourceCPU] = resource.MustParse("42")
	})

	classPersistence := newFullAgentClass().Spec.Persistence
	checkDeepCopy(t, "AgentClassPersistence/direct", &classPersistence, (*AgentClassPersistence).DeepCopy, func(p *AgentClassPersistence) {
		*p.StorageClassName = mutatedStr
	})

	classNetwork := newFullAgentClass().Spec.Network
	checkDeepCopy(t, "AgentClassNetwork/direct", &classNetwork, (*AgentClassNetwork).DeepCopy, func(n *AgentClassNetwork) {
		n.Egress.AllowedCIDRs[0] = mutatedStr
	})

	classEgress := newFullAgentClass().Spec.Network.Egress
	checkDeepCopy(t, "AgentClassEgress/direct", &classEgress, (*AgentClassEgress).DeepCopy, func(e *AgentClassEgress) {
		e.AllowedCIDRs[0] = mutatedStr
		e.AllowedHosts[0] = mutatedStr
	})

	classSecurity := newFullAgentClass().Spec.Security
	checkDeepCopy(t, "AgentClassSecurity/direct", &classSecurity, (*AgentClassSecurity).DeepCopy, func(s *AgentClassSecurity) {
		*s.PodSecurityContext.RunAsNonRoot = false
		*s.ContainerSecurityContext.ReadOnlyRootFilesystem = false
	})

	classLifecycle := newFullAgentClass().Spec.Lifecycle
	checkDeepCopy(t, "AgentClassLifecycle/direct", &classLifecycle, (*AgentClassLifecycle).DeepCopy, func(l *AgentClassLifecycle) {
		*l.TerminationGracePeriodSeconds = 1
	})

	classPodMetadata := newFullAgentClass().Spec.PodMetadata
	checkDeepCopy(t, "AgentClassPodMetadata/direct", &classPodMetadata, (*AgentClassPodMetadata).DeepCopy, func(m *AgentClassPodMetadata) {
		m.Labels["team"] = mutatedStr
		m.Annotations["note"] = mutatedStr
	})

	taskSpec := newFullAgentTask().Spec
	checkDeepCopy(t, "AgentTaskSpec/direct", &taskSpec, (*AgentTaskSpec).DeepCopy, func(s *AgentTaskSpec) {
		s.Env[0].Value = mutatedStr
		s.Providers[0].ProviderRef.Name = mutatedStr
		*s.Persistence.SizeGi = 999
		s.Artifacts[0].Name = mutatedStr
		*s.TTLSecondsAfterFinished = 1
	})

	taskStatus := newFullAgentTask().Status
	checkDeepCopy(t, "AgentTaskStatus/direct", &taskStatus, (*AgentTaskStatus).DeepCopy, func(s *AgentTaskStatus) {
		s.Conditions[0].Message = mutatedStr
		*s.StartTime = metav1.NewTime(time.Unix(999, 0))
		*s.CompletionTime = metav1.NewTime(time.Unix(999, 0))
		s.ArtifactValues["output-1"] = mutatedStr
	})

	taskPersistence := newFullAgentTask().Spec.Persistence
	checkDeepCopy(t, "AgentTaskPersistence/direct", &taskPersistence, (*AgentTaskPersistence).DeepCopy, func(p *AgentTaskPersistence) {
		*p.SizeGi = 1
	})

	providerSpec := newFullModelProvider().Spec
	checkDeepCopy(t, "ModelProviderSpec/direct", &providerSpec, (*ModelProviderSpec).DeepCopy, func(s *ModelProviderSpec) {
		s.Models[0].DisplayName = mutatedStr
		s.AllowedNamespaces[0] = mutatedStr
		s.Budget.Policies[0].Action = mutatedStr
		s.Fallback[0].Name = mutatedStr
		s.HealthCheck.IntervalSeconds = 1
	})

	providerStatus := newFullModelProvider().Status
	checkDeepCopy(t, "ModelProviderStatus/direct", &providerStatus, (*ModelProviderStatus).DeepCopy, func(s *ModelProviderStatus) {
		s.Conditions[0].Message = mutatedStr
		s.BudgetUsage[0].SpentUSD = mutatedStr
	})

	budget := newFullModelProvider().Spec.Budget
	checkDeepCopy(t, "ModelProviderBudget/direct", &budget, (*ModelProviderBudget).DeepCopy, func(b *ModelProviderBudget) {
		b.Policies[0].Action = mutatedStr
		*b.ClusterUSD = mutatedStr
	})

	policy := newFullModelProvider().Spec.Budget.Policies[1]
	checkDeepCopy(t, "ModelProviderBudgetPolicy/direct", &policy, (*ModelProviderBudgetPolicy).DeepCopy, func(p *ModelProviderBudgetPolicy) {
		*p.DegradeTo = mutatedStr
	})
}

// ---------------------------------------------------------------------------
// Leaf types whose DeepCopyInto is never invoked by a parent's DeepCopyInto
// (parents copy them by value or via `copy()`/`**out = **in`), so they need a
// direct exercise of their own DeepCopy method.
// ---------------------------------------------------------------------------

func TestLeafTypesDeepCopy(t *testing.T) {
	checkDeepCopy(t, "LocalObjectReference", &LocalObjectReference{Name: "ref-1"}, (*LocalObjectReference).DeepCopy, func(r *LocalObjectReference) {
		r.Name = mutatedStr
	})

	checkDeepCopy(t, "SecretKeyReference", &SecretKeyReference{Name: "secret-1", Key: "key-1"}, (*SecretKeyReference).DeepCopy, func(r *SecretKeyReference) {
		r.Name = mutatedStr
		r.Key = mutatedStr
	})

	checkDeepCopy(t, "AgentMCPServer", &AgentMCPServer{Name: "mcp1", URL: "https://mcp.example.com"}, (*AgentMCPServer).DeepCopy, func(s *AgentMCPServer) {
		s.Name = mutatedStr
		s.URL = mutatedStr
	})

	checkDeepCopy(t, "AgentProviderReference", &AgentProviderReference{ProviderRef: LocalObjectReference{Name: "provider-a"}}, (*AgentProviderReference).DeepCopy, func(p *AgentProviderReference) {
		p.ProviderRef.Name = mutatedStr
	})

	checkDeepCopy(t, "AgentTaskArtifact", &AgentTaskArtifact{Name: "artifact-1"}, (*AgentTaskArtifact).DeepCopy, func(a *AgentTaskArtifact) {
		a.Name = mutatedStr
	})

	checkDeepCopy(t, "ModelProviderModel", &ModelProviderModel{ID: "model-1", DisplayName: "Model 1", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "2.00"}, (*ModelProviderModel).DeepCopy, func(m *ModelProviderModel) {
		m.DisplayName = mutatedStr
	})

	checkDeepCopy(t, "ModelProviderBudgetUsage", &ModelProviderBudgetUsage{Namespace: "default", Period: "monthly", SpentUSD: "1.00", PercentUsed: 10, State: "Normal"}, (*ModelProviderBudgetUsage).DeepCopy, func(u *ModelProviderBudgetUsage) {
		u.State = mutatedStr
	})

	checkDeepCopy(t, "AgentService", &AgentService{Enabled: true, Port: 8080}, (*AgentService).DeepCopy, func(s *AgentService) {
		s.Port = 9999
	})

	checkDeepCopy(t, "ModelProviderHealthCheck", &ModelProviderHealthCheck{Enabled: true, IntervalSeconds: 30, TimeoutSeconds: 5}, (*ModelProviderHealthCheck).DeepCopy, func(h *ModelProviderHealthCheck) {
		h.IntervalSeconds = 999
	})

	checkDeepCopy(t, "AgentChannelSession", &AgentChannelSession{Enabled: true}, (*AgentChannelSession).DeepCopy, func(s *AgentChannelSession) {
		s.Enabled = false
	})

	checkDeepCopy(t, "AgentLifecycle", &AgentLifecycle{
		IdleTimeout:        metav1.Duration{Duration: time.Minute},
		HibernationEnabled: true,
		HibernationDelay:   metav1.Duration{Duration: 2 * time.Minute},
		ActivitySource:     "both",
		WakeTimeout:        metav1.Duration{Duration: 3 * time.Minute},
	}, (*AgentLifecycle).DeepCopy, func(l *AgentLifecycle) {
		l.ActivitySource = mutatedStr
	})

	checkDeepCopy(t, "AgentTaskCompletion", &AgentTaskCompletion{
		Condition:    "agentReported",
		Timeout:      metav1.Duration{Duration: time.Hour},
		OnTimeout:    "Fail",
		BackoffLimit: 3,
	}, (*AgentTaskCompletion).DeepCopy, func(c *AgentTaskCompletion) {
		c.OnTimeout = "Succeed"
	})

	checkDeepCopy(t, "ModelProviderRateLimits", &ModelProviderRateLimits{RequestsPerMinute: 100, TokensPerMinute: 1000}, (*ModelProviderRateLimits).DeepCopy, func(r *ModelProviderRateLimits) {
		r.RequestsPerMinute = 999
	})
}

// ---------------------------------------------------------------------------
// Nil-receiver checks for every exported DeepCopy/DeepCopyObject method in
// the package, so the generated `if in == nil { return nil }` guards are
// exercised for every type, not just the ones with a distinct fixture above.
// ---------------------------------------------------------------------------

func TestDeepCopyNilReceivers(t *testing.T) {
	checkNilDeepCopy(t, "Agent", (*Agent).DeepCopy)
	checkNilDeepCopy(t, "AgentChannel", (*AgentChannel).DeepCopy)
	checkNilDeepCopy(t, "AgentChannelList", (*AgentChannelList).DeepCopy)
	checkNilDeepCopy(t, "AgentChannelSession", (*AgentChannelSession).DeepCopy)
	checkNilDeepCopy(t, "AgentChannelSpec", (*AgentChannelSpec).DeepCopy)
	checkNilDeepCopy(t, "AgentChannelStatus", (*AgentChannelStatus).DeepCopy)
	checkNilDeepCopy(t, "AgentChannelWebhook", (*AgentChannelWebhook).DeepCopy)
	checkNilDeepCopy(t, "AgentClass", (*AgentClass).DeepCopy)
	checkNilDeepCopy(t, "AgentClassEgress", (*AgentClassEgress).DeepCopy)
	checkNilDeepCopy(t, "AgentClassImage", (*AgentClassImage).DeepCopy)
	checkNilDeepCopy(t, "AgentClassLifecycle", (*AgentClassLifecycle).DeepCopy)
	checkNilDeepCopy(t, "AgentClassList", (*AgentClassList).DeepCopy)
	checkNilDeepCopy(t, "AgentClassNetwork", (*AgentClassNetwork).DeepCopy)
	checkNilDeepCopy(t, "AgentClassPersistence", (*AgentClassPersistence).DeepCopy)
	checkNilDeepCopy(t, "AgentClassPodMetadata", (*AgentClassPodMetadata).DeepCopy)
	checkNilDeepCopy(t, "AgentClassResources", (*AgentClassResources).DeepCopy)
	checkNilDeepCopy(t, "AgentClassRuntime", (*AgentClassRuntime).DeepCopy)
	checkNilDeepCopy(t, "AgentClassSecurity", (*AgentClassSecurity).DeepCopy)
	checkNilDeepCopy(t, "AgentClassSpec", (*AgentClassSpec).DeepCopy)
	checkNilDeepCopy(t, "AgentClassStatus", (*AgentClassStatus).DeepCopy)
	checkNilDeepCopy(t, "AgentLifecycle", (*AgentLifecycle).DeepCopy)
	checkNilDeepCopy(t, "AgentList", (*AgentList).DeepCopy)
	checkNilDeepCopy(t, "AgentMCPServer", (*AgentMCPServer).DeepCopy)
	checkNilDeepCopy(t, "AgentPersistence", (*AgentPersistence).DeepCopy)
	checkNilDeepCopy(t, "AgentProviderReference", (*AgentProviderReference).DeepCopy)
	checkNilDeepCopy(t, "AgentService", (*AgentService).DeepCopy)
	checkNilDeepCopy(t, "AgentSpec", (*AgentSpec).DeepCopy)
	checkNilDeepCopy(t, "AgentStatus", (*AgentStatus).DeepCopy)
	checkNilDeepCopy(t, "AgentTask", (*AgentTask).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskArtifact", (*AgentTaskArtifact).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskCompletion", (*AgentTaskCompletion).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskList", (*AgentTaskList).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskPersistence", (*AgentTaskPersistence).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskSpec", (*AgentTaskSpec).DeepCopy)
	checkNilDeepCopy(t, "AgentTaskStatus", (*AgentTaskStatus).DeepCopy)
	checkNilDeepCopy(t, "ChannelAuth", (*ChannelAuth).DeepCopy)
	checkNilDeepCopy(t, "ChannelExtractor", (*ChannelExtractor).DeepCopy)
	checkNilDeepCopy(t, "ChannelHMAC", (*ChannelHMAC).DeepCopy)
	checkNilDeepCopy(t, "LocalObjectReference", (*LocalObjectReference).DeepCopy)
	checkNilDeepCopy(t, "ModelProvider", (*ModelProvider).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderBudget", (*ModelProviderBudget).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderBudgetPolicy", (*ModelProviderBudgetPolicy).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderBudgetUsage", (*ModelProviderBudgetUsage).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderHealthCheck", (*ModelProviderHealthCheck).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderList", (*ModelProviderList).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderModel", (*ModelProviderModel).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderRateLimits", (*ModelProviderRateLimits).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderSpec", (*ModelProviderSpec).DeepCopy)
	checkNilDeepCopy(t, "ModelProviderStatus", (*ModelProviderStatus).DeepCopy)
	checkNilDeepCopy(t, "SecretKeyReference", (*SecretKeyReference).DeepCopy)

	checkNilDeepCopyObject(t, "Agent", (*Agent).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentChannel", (*AgentChannel).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentChannelList", (*AgentChannelList).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentClass", (*AgentClass).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentClassList", (*AgentClassList).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentList", (*AgentList).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentTask", (*AgentTask).DeepCopyObject)
	checkNilDeepCopyObject(t, "AgentTaskList", (*AgentTaskList).DeepCopyObject)
	checkNilDeepCopyObject(t, "ModelProvider", (*ModelProvider).DeepCopyObject)
	checkNilDeepCopyObject(t, "ModelProviderList", (*ModelProviderList).DeepCopyObject)
}

// ---------------------------------------------------------------------------
// Package-level plumbing: constants and scheme registration.
// ---------------------------------------------------------------------------

func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "agentry.io" {
		t.Fatalf("GroupVersion.Group = %q, want agentry.io", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Fatalf("GroupVersion.Version = %q, want v1alpha1", GroupVersion.Version)
	}
	if AddToScheme == nil {
		t.Fatal("AddToScheme is nil")
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	for _, obj := range []runtime.Object{
		&Agent{}, &AgentList{},
		&AgentChannel{}, &AgentChannelList{},
		&AgentClass{}, &AgentClassList{},
		&AgentTask{}, &AgentTaskList{},
		&ModelProvider{}, &ModelProviderList{},
	} {
		kinds, _, err := scheme.ObjectKinds(obj)
		if err != nil || len(kinds) == 0 {
			t.Fatalf("scheme does not recognize %T: kinds=%v err=%v", obj, kinds, err)
		}
		for _, gvk := range kinds {
			if gvk.GroupVersion() != GroupVersion {
				t.Fatalf("%T registered under %v, want %v", obj, gvk.GroupVersion(), GroupVersion)
			}
		}
	}
}

func TestSessionNamespaceUUIDConstant(t *testing.T) {
	if SessionNamespaceUUID != "f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d" {
		t.Fatalf("SessionNamespaceUUID changed value: %q", SessionNamespaceUUID)
	}
}

func TestSANConstants(t *testing.T) {
	if AgentSANSuffix != "svc.cluster.local" {
		t.Fatalf("AgentSANSuffix = %q", AgentSANSuffix)
	}
	if TaskSANSuffix != "task.agentry.io" {
		t.Fatalf("TaskSANSuffix = %q", TaskSANSuffix)
	}
	if AgentSANLabels != 5 {
		t.Fatalf("AgentSANLabels = %d, want 5", AgentSANLabels)
	}
	if TaskSANLabels != 5 {
		t.Fatalf("TaskSANLabels = %d, want 5", TaskSANLabels)
	}
}
