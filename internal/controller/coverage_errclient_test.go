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
	"context"
	"fmt"
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cmapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// newErrListClient returns a client whose List always fails, so the many
// map-func and reference-count error branches can be exercised deterministically.
func newErrListClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return fmt.Errorf("injected list failure")
		},
	}).Build()
}

// newErrGetClient returns a client whose Get always fails with a non-NotFound
// error, to exercise the Get error branches.
func newErrGetClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return fmt.Errorf("injected get failure")
		},
	}).Build()
}

// newErrCreateClient returns a client whose Create always fails with a
// non-AlreadyExists error, to exercise the child-convergence error branches.
func newErrCreateClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return fmt.Errorf("injected create failure")
		},
	}).Build()
}

func TestEnsureChildren_CreateErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	c := newErrCreateClient(t)

	agent := &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
	class := &agentryv1alpha1.AgentClass{}
	ar := &AgentReconciler{Client: c, OperatorNamespace: "agentry-system"}
	eff := effectiveAgentSpec{HealthPort: 8080, ServicePort: 8080, ServiceEnabled: true, PersistenceOn: true, PVCSizeGi: 1}

	if err := ar.ensureServiceAccount(ctx, agent); err == nil {
		t.Error("ensureServiceAccount must surface a create error")
	}
	if err := ar.ensureService(ctx, agent, eff); err == nil {
		t.Error("ensureService must surface a create error")
	}
	if err := ar.ensurePVC(ctx, agent, class, eff); err == nil {
		t.Error("ensurePVC must surface a create error")
	}
	if err := ar.ensureNetworkPolicy(ctx, agent, class, eff); err == nil {
		t.Error("ensureNetworkPolicy must surface a create error")
	}
	// The Certificate does not exist yet, so ensureCertificate takes the create
	// path and surfaces the failure.
	if _, err := ar.ensureCertificate(ctx, agent); err == nil {
		t.Error("ensureCertificate must surface a create error")
	}
	// convergePod finds no Pod and fails to create one.
	if err := ar.convergePod(ctx, agent, eff); err == nil {
		t.Error("convergePod must surface a create error")
	}

	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"}}
	tr := &AgentTaskReconciler{Client: c, OperatorNamespace: "agentry-system"}
	if err := tr.ensureTaskChildren(ctx, task, class, effectiveTaskSpec{PersistenceOn: true, PVCSizeGi: 1}); err == nil {
		t.Error("ensureTaskChildren must surface a create error")
	}
	if _, err := tr.ensureTaskCertificate(ctx, task); err == nil {
		t.Error("ensureTaskCertificate must surface a create error")
	}
}

func TestGetErrorBranches(t *testing.T) {
	ctx := context.Background()
	c := newErrGetClient(t)

	// credential surfaces a non-NotFound Secret Get error as CredentialsMissing.
	r := &ModelProviderReconciler{Client: c, OperatorNamespace: "default"}
	mp := &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       agentryv1alpha1.ModelProviderSpec{CredentialsRef: agentryv1alpha1.SecretKeyReference{Name: "s", Key: "token"}},
	}
	_, reason, msg := r.credential(ctx, mp)
	if reason != agentryv1alpha1.ReasonCredentialsMissing || msg == "" {
		t.Errorf("credential Get error: reason=%q msg=%q", reason, msg)
	}

	// reconcileBudget surfaces a non-NotFound ConfigMap Get error.
	budgeted := &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       agentryv1alpha1.ModelProviderSpec{Budget: agentryv1alpha1.ModelProviderBudget{Period: "monthly"}},
	}
	if err := r.reconcileBudget(ctx, budgeted, map[string]bool{}); err == nil {
		t.Error("reconcileBudget must surface a ConfigMap Get error")
	}
}

func TestMapFuncs_ListErrorReturnsNil(t *testing.T) {
	ctx := context.Background()
	c := newErrListClient(t)

	ar := &AgentReconciler{Client: c}
	if reqs := ar.agentsForClass(ctx, &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}); reqs != nil {
		t.Errorf("agentsForClass on a list error must return nil: %v", reqs)
	}
	if reqs := ar.agentsForProvider(ctx, &agentryv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}); reqs != nil {
		t.Errorf("agentsForProvider on a list error must return nil: %v", reqs)
	}

	acr := &AgentClassReconciler{Client: c}
	if reqs := acr.classesForProvider(ctx, &agentryv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}); reqs != nil {
		t.Errorf("classesForProvider on a list error must return nil: %v", reqs)
	}

	tr := &AgentTaskReconciler{Client: c}
	if reqs := tr.tasksForClass(ctx, &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}); reqs != nil {
		t.Errorf("tasksForClass on a list error must return nil: %v", reqs)
	}

	chr := &AgentChannelReconciler{Client: c}
	if reqs := chr.channelsForAgent(ctx, &agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}); reqs != nil {
		t.Errorf("channelsForAgent on a list error must return nil: %v", reqs)
	}
}

func TestReferenceCountsPropagateListErrors(t *testing.T) {
	ctx := context.Background()
	c := newErrListClient(t)

	// gatewayPods surfaces the list error.
	if _, _, err := (&ModelProviderReconciler{Client: c, OperatorNamespace: "default"}).gatewayPods(ctx); err == nil {
		t.Error("gatewayPods must surface a list error")
	}

	// isReferenced (via reconcileDelete on a finalized provider) surfaces it too.
	mp := &agentryv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	controllerutil.AddFinalizer(mp, agentryv1alpha1.ProviderFinalizer)
	if _, err := (&ModelProviderReconciler{Client: c}).reconcileDelete(ctx, mp); err == nil {
		t.Error("provider reconcileDelete must surface the isReferenced list error")
	}

	// countUsers (via reconcileDelete on a finalized class) surfaces it.
	ac := &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	controllerutil.AddFinalizer(ac, agentryv1alpha1.ClassFinalizer)
	if _, err := (&AgentClassReconciler{Client: c}).reconcileDelete(ctx, ac); err == nil {
		t.Error("class reconcileDelete must surface the countUsers list error")
	}
}
