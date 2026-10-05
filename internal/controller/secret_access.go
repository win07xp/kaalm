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
	"fmt"
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

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const (
	// controllerServiceAccount is the operator's own ServiceAccount name, the
	// subject of every Role the reconcilers mint for their own Secret reads.
	controllerServiceAccount = "kaalm-controller"

	// kindRole is the RoleRef kind of every RoleBinding the reconcilers mint.
	kindRole = "Role"

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

func agentEnvSecretRoleName(agentName string) string {
	return "kaalm-agent-" + agentName + "-envsecrets"
}

func taskEnvSecretRoleName(taskName string) string {
	return "kaalm-task-" + taskName + "-envsecrets"
}

// envSecretRefs lists the distinct Secret names a workload's env reads
// through valueFrom.secretKeyRef, sorted. Literal values and the other
// valueFrom sources name no Secret; an empty name is left out, so it never
// reaches a Role (checkEnvSecrets reports it).
func envSecretRefs(env []corev1.EnvVar) []corev1.LocalObjectReference {
	seen := map[string]bool{}
	var names []string
	for _, e := range env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		name := e.ValueFrom.SecretKeyRef.Name
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return refs
}

// envSecretNotUsableMessage is the rule 48 status message. A missing Secret
// and an unlabeled one read the same, so status never tells a developer
// whether a Secret name exists, and it names no key or value.
func envSecretNotUsableMessage(envName, secretName string) string {
	return fmt.Sprintf("env %q: Secret %q is not usable: a workload may use only a Secret in its namespace "+
		"that carries the label %s: %q", envName, secretName, kaalmv1beta1.LabelWorkloadSecret, kaalmv1beta1.AnnotationTrue)
}

// checkEnvSecrets enforces rule 48 on a workload's env: every Secret a
// valueFrom.secretKeyRef names must exist in namespace and carry
// LabelWorkloadSecret: "true", optional references included. It walks env in
// spec order and reports the first failure as a status reason and message:
// InvalidReference for an empty name, SecretNotOptedIn for a missing or
// unlabeled Secret. Any other read error, a lasting Forbidden included, is
// returned as err. Each distinct Secret is read once.
func checkEnvSecrets(
	ctx context.Context, reader client.Reader, namespace string, env []corev1.EnvVar,
) (reason, msg string, err error) {
	checked := map[string]bool{}
	for _, e := range env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		name := e.ValueFrom.SecretKeyRef.Name
		if name == "" {
			return kaalmv1beta1.ReasonInvalidReference, fmt.Sprintf("env %q: secretKeyRef names no Secret", e.Name), nil
		}
		if checked[name] {
			continue
		}
		var sec corev1.Secret
		err := getSecretLive(ctx, reader, types.NamespacedName{Namespace: namespace, Name: name}, &sec)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", "", err
		}
		if err != nil || !kaalmv1beta1.WorkloadSecretOptedIn(sec.Labels) {
			return kaalmv1beta1.ReasonSecretNotOptedIn, envSecretNotUsableMessage(e.Name, name), nil
		}
		checked[name] = true
	}
	return "", "", nil
}

// liveSecretReader picks the reader for a Secret outside the operator
// namespace. The manager's cache holds Secrets of the operator namespace only,
// so in production reader is the secretwatch reader: one name-filtered watch
// per referenced Secret. reader is nil in tests that build a reconciler around
// one client.
func liveSecretReader(reader client.Reader, fallback client.Reader) client.Reader {
	if reader != nil {
		return reader
	}
	return fallback
}

// getSecretLive reads one Secret in a user namespace, retrying a Forbidden
// answer while the Role that grants the read reaches the authorizer. With the
// secretwatch reader a synced read is a cache hit; an unsynced one goes to
// the apiserver, so a Forbidden still reaches this retry.
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

// ensureControllerSecretAccess keeps a controller-only Role and RoleBinding,
// both named roleName and owned by owner: get and watch on exactly the named
// Secrets (watch lets the controller's per-Secret watch sync), bound to the
// operator's ServiceAccount alone. With no names the pair is removed, so an
// edit that drops its Secret references leaves no grant behind. A Role or
// RoleBinding of that name the owner does not control is a ChildConflictError,
// never updated or deleted. It serves the rule 23 pull-Secret check of Agents
// and AgentTasks, the rule 48 env-Secret label check of Agents and
// AgentTasks (a Role of its own, apart from the pull-Secret one), and the
// rule 45 label check of AgentChannels.
func ensureControllerSecretAccess(
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
	if found {
		if err := requireControlled(scheme, owner, &current); err != nil {
			return err
		}
	}
	var currentRB rbacv1.RoleBinding
	rbErr := c.Get(ctx, key, &currentRB)
	if rbErr != nil && !apierrors.IsNotFound(rbErr) {
		return rbErr
	}
	rbFound := rbErr == nil
	if rbFound {
		if err := requireControlled(scheme, owner, &currentRB); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		// The RoleBinding goes first: a binding to a missing Role grants
		// nothing, a Role with no binding is only clutter.
		if rbFound {
			if err := c.Delete(ctx, &currentRB); err != nil && !apierrors.IsNotFound(err) {
				return rejectedWrite("deleting", scheme, &currentRB, err)
			}
		}
		if !found {
			return nil
		}
		return rejectedWrite("deleting", scheme, &current, client.IgnoreNotFound(c.Delete(ctx, &current)))
	}

	rules := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: names,
		Verbs:         []string{"get", "watch"},
	}}
	if !found {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: key.Namespace}, Rules: rules}
		if err := controllerutil.SetControllerReference(owner, role, scheme); err != nil {
			return err
		}
		if err := createControlled(ctx, c, owner, role); err != nil {
			return err
		}
	} else if !equality.Semantic.DeepEqual(current.Rules, rules) {
		current.Rules = rules
		if err := c.Update(ctx, &current); err != nil {
			return rejectedWrite("updating", scheme, &current, err)
		}
	}

	if rbFound {
		return nil
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: key.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindRole, Name: roleName},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: controllerServiceAccount, Namespace: operatorNamespace,
		}},
	}
	if err := controllerutil.SetControllerReference(owner, rb, scheme); err != nil {
		return err
	}
	return createControlled(ctx, c, owner, rb)
}
