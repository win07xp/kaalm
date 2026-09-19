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
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// controllerServiceAccount is the operator's own ServiceAccount name, the
	// subject of every Role the reconcilers mint for their own Secret reads.
	controllerServiceAccount = "kaalm-controller"

	// A Role the reconciler created a moment ago can be missing from the
	// apiserver's authorizer for a few hundred milliseconds. A Forbidden read
	// is retried inside the pass that long before it is reported.
	secretReadAttempts = 5
	secretReadBackoff  = 200 * time.Millisecond
)

func agentPullSecretRoleName(agentName string) string {
	return "kaalm-agent-" + agentName + "-pullsecrets"
}

func taskPullSecretRoleName(taskName string) string {
	return "kaalm-task-" + taskName + "-pullsecrets"
}

// liveSecretReader picks the reader for a Secret outside the operator
// namespace. The manager's cache holds Secrets of the operator namespace only,
// so these reads go to the apiserver; reader is nil in tests that build a
// reconciler around one client.
func liveSecretReader(reader client.Reader, fallback client.Reader) client.Reader {
	if reader != nil {
		return reader
	}
	return fallback
}

// getSecretLive reads one Secret in a user namespace, retrying a Forbidden
// answer while the Role that grants the read reaches the authorizer.
func getSecretLive(ctx context.Context, reader client.Reader, key types.NamespacedName, sec *corev1.Secret) error {
	var err error
	for attempt := 0; attempt < secretReadAttempts; attempt++ {
		if err = reader.Get(ctx, key, sec); !apierrors.IsForbidden(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(secretReadBackoff):
		}
	}
	return err
}

// ensurePullSecretAccess keeps the Role and RoleBinding that let the operator
// confirm rule 23 for one workload: get on exactly the Secrets the class
// names, bound to the operator's ServiceAccount, owned by the workload. With
// no names the pair is removed, so a class edit that drops its pull Secrets
// leaves no grant behind.
func ensurePullSecretAccess(
	ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object,
	roleName, operatorNamespace string, refs []corev1.LocalObjectReference,
) error {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	key := types.NamespacedName{Namespace: owner.GetNamespace(), Name: roleName}

	var current rbacv1.Role
	err := c.Get(ctx, key, &current)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	found := err == nil
	if len(names) == 0 {
		if !found {
			return nil
		}
		// The RoleBinding goes first: a binding to a missing Role grants
		// nothing, a Role with no binding is only clutter.
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: key.Namespace}}
		if err := c.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return client.IgnoreNotFound(c.Delete(ctx, &current))
	}

	rules := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: names,
		Verbs:         []string{"get"},
	}}
	if !found {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: key.Namespace}, Rules: rules}
		if err := controllerutil.SetControllerReference(owner, role, scheme); err != nil {
			return err
		}
		if err := c.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if !equality.Semantic.DeepEqual(current.Rules, rules) {
		current.Rules = rules
		if err := c.Update(ctx, &current); err != nil {
			return err
		}
	}

	var currentRB rbacv1.RoleBinding
	if err := c.Get(ctx, key, &currentRB); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: key.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: controllerServiceAccount, Namespace: operatorNamespace,
		}},
	}
	if err := controllerutil.SetControllerReference(owner, rb, scheme); err != nil {
		return err
	}
	if err := c.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
