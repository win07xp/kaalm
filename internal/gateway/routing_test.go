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

package gateway

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestSplitQualifiedModel(t *testing.T) {
	cases := []struct {
		in              string
		provider, model string
		ok              bool
	}{
		{"anthropic-shared/claude-opus-4-6", "anthropic-shared", "claude-opus-4-6", true},
		{"local-vllm/org/llama-3-70b", "local-vllm", "org/llama-3-70b", true}, // split on FIRST slash
		{"no-slash", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		p, m, ok := splitQualifiedModel(c.in)
		if ok != c.ok || p != c.provider || m != c.model {
			t.Errorf("splitQualifiedModel(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, m, ok, c.provider, c.model, c.ok)
		}
	}
}

func TestAdapterUsageExtraction(t *testing.T) {
	a := anthropicAdapter{}
	u, ok := a.extractUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":25}}`))
	if !ok || u.InputTokens != 100 || u.OutputTokens != 25 {
		t.Errorf("anthropic buffered usage wrong: %+v ok=%v", u, ok)
	}
	o := openaiAdapter{}
	u, ok = o.extractUsage([]byte(`{"usage":{"prompt_tokens":50,"completion_tokens":10}}`))
	if !ok || u.InputTokens != 50 || u.OutputTokens != 10 {
		t.Errorf("openai buffered usage wrong: %+v ok=%v", u, ok)
	}
	if _, ok := o.extractUsage([]byte(`{"choices":[]}`)); ok {
		t.Error("no usage object must report ok=false")
	}
}

func TestAnthropicStreamUsage(t *testing.T) {
	a := anthropicAdapter{}
	var u Usage
	a.accumulateStreamUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":42}}}`), &u)
	a.accumulateStreamUsage([]byte(`{"type":"content_block_delta"}`), &u)
	a.accumulateStreamUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":17}}`), &u)
	a.accumulateStreamUsage([]byte(`{"type":"message_stop"}`), &u)
	if u.InputTokens != 42 || u.OutputTokens != 17 {
		t.Errorf("anthropic stream usage wrong: %+v", u)
	}
}

func TestOpenAIStreamUsageAndFixup(t *testing.T) {
	o := openaiAdapter{}
	var u Usage
	o.accumulateStreamUsage([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`), &u)
	o.accumulateStreamUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`), &u)
	o.accumulateStreamUsage([]byte(`[DONE]`), &u)
	if u.InputTokens != 9 || u.OutputTokens != 4 {
		t.Errorf("openai stream usage wrong: %+v", u)
	}

	body := map[string]any{"stream": true}
	o.fixupRequestBody(body)
	if _, ok := body["stream_options"]; !ok {
		t.Error("include_usage fixup not injected")
	}
	// Absent stream flag: no injection.
	body = map[string]any{}
	o.fixupRequestBody(body)
	if _, ok := body["stream_options"]; ok {
		t.Error("fixup must not fire on non-streaming requests")
	}
	// Caller-provided stream_options preserved.
	body = map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": false}}
	o.fixupRequestBody(body)
	if body["stream_options"].(map[string]any)["include_usage"] != false {
		t.Error("caller-provided stream_options must be preserved")
	}
}

func TestNamespaceGlobAllowed(t *testing.T) {
	if !namespaceGlobAllowed("team-a", []string{"team-*"}) || !namespaceGlobAllowed("x", nil) {
		t.Error("glob and empty-list semantics wrong")
	}
	if namespaceGlobAllowed("prod", []string{"team-*", "dev"}) {
		t.Error("non-matching namespace admitted")
	}
}

func TestWorkloadProviders(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.agents["team-a/sup"] = &kaalmv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Spec: kaalmv1alpha1.AgentSpec{
			AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: "std"},
			Providers:     []kaalmv1alpha1.AgentProviderReference{{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "prov"}}},
		},
	}
	fs.tasks["team-a/fix"] = &kaalmv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "team-a"},
		Spec: kaalmv1alpha1.AgentTaskSpec{
			AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: "tstd"},
			Providers:     []kaalmv1alpha1.AgentProviderReference{{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "tprov"}}},
		},
	}
	ctx := context.Background()

	refs, class, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "sup", Kind: KindAgent}})
	if !ok || class != "std" || len(refs) != 1 || refs[0].ProviderRef.Name != "prov" {
		t.Errorf("agent providers wrong: %v %q %v", refs, class, ok)
	}
	refs, class, ok = s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "fix", Kind: KindAgentTask}})
	if !ok || class != "tstd" || refs[0].ProviderRef.Name != "tprov" {
		t.Errorf("task providers wrong: %v %q %v", refs, class, ok)
	}
	// Unknown agent.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "ghost", Kind: KindAgent}}); ok {
		t.Error("unknown agent must miss")
	}
	// Unknown task.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "ghost", Kind: KindAgentTask}}); ok {
		t.Error("unknown task must miss")
	}
	// Unrecognized kind.
	if _, _, ok := s.workloadProviders(ctx, &caller{Namespace: "team-a", Workload: &Identity{Name: "x", Kind: "Weird"}}); ok {
		t.Error("unrecognized kind must miss")
	}
}
