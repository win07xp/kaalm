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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// mkOpenTP creates a credential-less ToolProvider admitting every namespace,
// the minimal fixture for grant-rule tests.
func mkOpenTP(t *testing.T, name string, mutate func(*kaalmv1beta1.ToolProvider)) {
	t.Helper()
	mkToolProvider(t, name, func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.CredentialsRef = nil
		tp.Spec.AllowedNamespaces = []string{"*"}
		if mutate != nil {
			mutate(tp)
		}
	})
}

func grantOf(name string, tools ...string) kaalmv1beta1.AgentToolGrant {
	return kaalmv1beta1.AgentToolGrant{
		ProviderRef: kaalmv1beta1.LocalObjectReference{Name: name},
		Tools:       tools,
	}
}

func expectAgentNotDegraded(t *testing.T, name string) {
	t.Helper()
	// Errors return into the retry loop: testClient reads the manager cache,
	// so a just-created agent can be NotFound for the first few polls.
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag); err != nil {
			return err
		}
		if ag.Status.Phase == "" {
			return errString("no phase yet")
		}
		if ag.Status.Phase == kaalmv1beta1.AgentDegraded {
			return errString("still Degraded")
		}
		return nil
	})
}

// ---- rules 35 to 38 on the Agent (recoverable Degraded) ----

func TestAgentToolGrant_ClassAllowlistGateAndRecovery(t *testing.T) {
	mkOpenTP(t, "tg-r37", nil)
	mkWorkloadClass(t, "wc-r37", nil) // no allowedToolProviders: rule 37 denies
	mkWorkloadAgent(t, "r37-agent", "wc-r37", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-r37")}
	})
	expectAgentPhase(t, "r37-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "r37-agent", kaalmv1beta1.ReasonClassConstraintViolation)

	// The platform team allowlists the provider on the class: recovery.
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "wc-r37"}, &ac); err != nil {
			return err
		}
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-r37"}}
		return testClient.Update(ctxT(), &ac)
	})
	expectAgentNotDegraded(t, "r37-agent")
}

func TestAgentToolGrant_MissingProviderGateAndRecovery(t *testing.T) {
	mkWorkloadClass(t, "wc-r35", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-r35"}}
	})
	mkWorkloadAgent(t, "r35-agent", "wc-r35", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-r35")}
	})
	expectAgentPhase(t, "r35-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "r35-agent", kaalmv1beta1.ReasonClassConstraintViolation)

	// Creating the ToolProvider recovers the agent through the ToolProvider
	// watch: nothing else re-enqueues it.
	mkOpenTP(t, "tg-r35", nil)
	expectAgentNotDegraded(t, "r35-agent")
}

func TestAgentToolGrant_NamespaceDeniedGateAndRecovery(t *testing.T) {
	mkToolProvider(t, "tg-r36", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.CredentialsRef = nil
		tp.Spec.AllowedNamespaces = []string{"team-*"} // agent lives in default
	})
	mkWorkloadClass(t, "wc-r36", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-r36"}}
	})
	mkWorkloadAgent(t, "r36-agent", "wc-r36", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-r36")}
	})
	expectAgentPhase(t, "r36-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "r36-agent", kaalmv1beta1.ReasonClassConstraintViolation)

	// Widening the provider's allowedNamespaces recovers the agent, proving
	// ToolProvider spec changes propagate.
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tg-r36"}, &tp); err != nil {
			return err
		}
		tp.Spec.AllowedNamespaces = []string{"*"}
		return testClient.Update(ctxT(), &tp)
	})
	expectAgentNotDegraded(t, "r36-agent")
}

func TestAgentToolGrant_EmptyAllowedNamespacesDeniesAll(t *testing.T) {
	// Since v0.4.0 an empty allowedNamespaces list allows none: "*" is the
	// explicit opt-in. This test locks the deny-on-empty semantics.
	mkToolProvider(t, "tg-empty", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.CredentialsRef = nil
		tp.Spec.AllowedNamespaces = nil
	})
	mkWorkloadClass(t, "wc-empty", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-empty"}}
	})
	mkWorkloadAgent(t, "empty-agent", "wc-empty", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-empty")}
	})
	expectAgentPhase(t, "empty-agent", kaalmv1beta1.AgentDegraded)
	ag := getWorkloadAgent(t, "empty-agent")
	c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil || !strings.Contains(c.Message, "does not allow namespace") {
		t.Fatalf("Ready message = %+v, want a namespace denial", c)
	}
}

func TestAgentToolGrant_CatalogGateAndRecovery(t *testing.T) {
	mkOpenTP(t, "tg-r38", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.Tools = []kaalmv1beta1.ToolProviderTool{{ID: "web_search"}}
	})
	mkWorkloadClass(t, "wc-r38", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-r38"}}
	})
	mkWorkloadAgent(t, "r38-agent", "wc-r38", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-r38", "fetch_page")}
	})
	expectAgentPhase(t, "r38-agent", kaalmv1beta1.AgentDegraded)
	expectAgentReadyReason(t, "r38-agent", kaalmv1beta1.ReasonToolNotInCatalog)
	ag := getWorkloadAgent(t, "r38-agent")
	c := condition(ag.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil || !strings.Contains(c.Message, "fetch_page") {
		t.Fatalf("Ready message = %+v, want it to name the missing tool", c)
	}

	// Declaring the tool in the catalog recovers the agent.
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tg-r38"}, &tp); err != nil {
			return err
		}
		tp.Spec.Tools = append(tp.Spec.Tools, kaalmv1beta1.ToolProviderTool{ID: "fetch_page"})
		return testClient.Update(ctxT(), &tp)
	})
	expectAgentNotDegraded(t, "r38-agent")
}

func TestAgentToolGrant_NoCatalogAcceptsAnyNarrowing(t *testing.T) {
	mkOpenTP(t, "tg-nocat", nil) // no declared catalog: rule 38 inapplicable
	mkWorkloadClass(t, "wc-nocat", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-nocat"}}
	})
	mkWorkloadAgent(t, "nocat-agent", "wc-nocat", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-nocat", "anything-goes")}
	})
	expectAgentNotDegraded(t, "nocat-agent")
}

// ---- the same rules on AgentTask (terminal Failed) ----

func TestTaskToolGrant_ViolationIsTerminalFailed(t *testing.T) {
	mkOpenTP(t, "tg-task", nil)
	mkWorkloadClass(t, "tc-tool", nil) // empty allowedToolProviders => rule 37 denies
	mkTask(t, "t-tool", "tc-tool", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-task")}
	})
	expectTaskPhase(t, "t-tool", kaalmv1beta1.TaskFailed)
	c := condition(getTask(t, "t-tool").Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Reason != kaalmv1beta1.ReasonClassConstraintViolation {
		t.Errorf("Completed condition wrong: %+v", c)
	}
}

func TestTaskToolGrant_ValidGrantProvisions(t *testing.T) {
	mkOpenTP(t, "tg-task-ok", nil)
	// Wait until the ToolProvider is reconciled (and therefore in the shared
	// informer cache) before creating the task: a task violation is terminal,
	// so racing the cache would fail the task on a phantom rule 35 miss,
	// exactly as it would for a ModelProvider created in the same instant.
	awaitToolProviderFinalizer(t, "tg-task-ok")
	mkWorkloadClass(t, "tc-tool-ok", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tg-task-ok"}}
	})
	mkTask(t, "t-tool-ok", "tc-tool-ok", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tg-task-ok")}
	})
	eventually(t, func() error {
		var task kaalmv1beta1.AgentTask
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-tool-ok"}, &task); err != nil {
			return err
		}
		switch task.Status.Phase {
		case kaalmv1beta1.TaskFailed:
			return errString("task failed on a valid grant")
		case "", kaalmv1beta1.TaskPending:
			return errString("no phase yet")
		}
		return nil
	})
}

// ---- the class side ----

func TestAgentClass_MissingToolProviderIsNotReadyAndRecovers(t *testing.T) {
	ac := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-tools"},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedToolProviders: []kaalmv1beta1.LocalObjectReference{{Name: "tg-cls"}},
		},
	}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class: %v", err)
	}
	get := func() []metav1.Condition {
		var got kaalmv1beta1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-tools"}, &got)
		return got.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonInvalidReference)

	// Creating the ToolProvider recovers the class through its watch.
	mkOpenTP(t, "tg-cls", nil)
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonAllReferencesResolved)
}
