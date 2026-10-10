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

package gateway

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestIsKaalmManagedPod(t *testing.T) {
	// OwnerRef to an Agent.
	agentPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kaalmv1beta1.GroupVersion.String(), Kind: "Agent"}},
	}}
	if !isKaalmManagedPod(agentPod) {
		t.Error("Agent-owned pod must be managed")
	}
	// OwnerRef to an AgentTask.
	taskPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kaalmv1beta1.GroupVersion.String(), Kind: "AgentTask"}},
	}}
	if !isKaalmManagedPod(taskPod) {
		t.Error("AgentTask-owned pod must be managed")
	}
	// OwnerRef written before the v0.6.0 graduation names v1alpha1; the
	// match is by group, so the Pod is still managed.
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "kaalm.io/v1alpha1", Kind: "Agent"}},
	}}
	if !isKaalmManagedPod(oldPod) {
		t.Error("pod owned through kaalm.io/v1alpha1 must be managed")
	}
	// Label-based.
	labeledPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kaalm.io/workload": "agent"}}}
	if !isKaalmManagedPod(labeledPod) {
		t.Error("labeled pod must be managed")
	}
	// Plain pod: not managed. An unrelated ownerRef must not match.
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet"}},
	}}
	if isKaalmManagedPod(plain) {
		t.Error("plain pod must not be managed")
	}
}

// storeSnapshot is a deep copy of every object a fakeStore serves.
type storeSnapshot struct {
	agents    map[string]*kaalmv1beta1.Agent
	classes   map[string]*kaalmv1beta1.AgentClass
	providers map[string]*kaalmv1beta1.ModelProvider
	tools     map[string]*kaalmv1beta1.ToolProvider
}

func snapshotStore(f *fakeStore) storeSnapshot {
	s := storeSnapshot{
		agents: map[string]*kaalmv1beta1.Agent{}, classes: map[string]*kaalmv1beta1.AgentClass{},
		providers: map[string]*kaalmv1beta1.ModelProvider{}, tools: map[string]*kaalmv1beta1.ToolProvider{},
	}
	for k, v := range f.agents {
		s.agents[k] = v.DeepCopy()
	}
	for k, v := range f.classes {
		s.classes[k] = v.DeepCopy()
	}
	for k, v := range f.providers {
		s.providers[k] = v.DeepCopy()
	}
	for k, v := range f.toolProviders {
		s.tools[k] = v.DeepCopy()
	}
	return s
}

// The gateway cache hands out request-path objects without a deep copy
// (cmd/gateway), which is safe only while no request path changes them.
// Every request shape that reads them leaves the store's objects as they
// were.
func TestRequestPathsLeaveStoreObjectsUnchanged(t *testing.T) {
	llmJSON := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	}
	failing := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	chat := map[string]any{"model": "prov/m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	cases := []struct {
		name  string
		setup func(t *testing.T) (*harness, *http.Client)
		path  string
		body  map[string]any
	}{
		{"buffered", func(t *testing.T) (*harness, *http.Client) {
			h := newHarness(t, llmJSON)
			h.seedRoute()
			cert := agentCert(t, h.ca)
			return h, h.client(&cert)
		}, "/v1/chat/completions", chat},
		{"streaming", func(t *testing.T) (*harness, *http.Client) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
			})
			h.seedRoute()
			cert := agentCert(t, h.ca)
			return h, h.client(&cert)
		}, "/v1/chat/completions", map[string]any{"model": "prov/m1", "stream": true, "messages": []any{}}},
		{"same-type fallback with a modelMap", func(t *testing.T) (*harness, *http.Client) {
			h := newHarness(t, failing)
			h.seedRoute()
			h.server.Recorder = &recordingRecorder{}
			h.addBackupProvider(t, llmJSON)
			h.store.providers["backup"].Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "m2"}}
			h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{
				Name: "backup", ModelMap: map[string]string{"m1": "m2"}}}
			h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
				kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
			h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
				kaalmv1beta1.LocalObjectReference{Name: "backup"})
			cert := agentCert(t, h.ca)
			return h, h.client(&cert)
		}, "/v1/chat/completions", chat},
		{"crossing fallback", func(t *testing.T) (*harness, *http.Client) {
			h, cert := crossingHarness(t, llmJSON)
			return h, h.client(cert)
		}, "/v1/messages", map[string]any{"model": "prov/m1", "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"tools/list", func(t *testing.T) (*harness, *http.Client) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"}]}}`)
			})
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			return h, h.client(&cert)
		}, "/v1/mcp/search", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}},
		{"tools/call", func(t *testing.T) (*harness, *http.Client) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`)
			})
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			return h, h.client(&cert)
		}, "/v1/mcp/search", mcpCall("web_search")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, client := c.setup(t)
			before := snapshotStore(h.store)
			resp := postJSON(t, client, h.url(c.path), c.body, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			after := snapshotStore(h.store)
			for _, kind := range []struct {
				name          string
				before, after any
			}{
				{"Agent", before.agents, after.agents},
				{"AgentClass", before.classes, after.classes},
				{"ModelProvider", before.providers, after.providers},
				{"ToolProvider", before.tools, after.tools},
			} {
				if !equality.Semantic.DeepEqual(kind.before, kind.after) {
					t.Errorf("the request changed a %s:\nbefore %+v\nafter  %+v", kind.name, kind.before, kind.after)
				}
			}
		})
	}
}
