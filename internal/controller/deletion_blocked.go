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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// referrerIndexes names the field indexes that find the objects pinning a
// cluster-scoped resource against deletion. An empty index is skipped: an
// AgentClass is pinned by workloads only.
type referrerIndexes struct {
	agent, task, class string
}

// listReferrers returns every Agent, AgentTask, and AgentClass whose index
// matches name, as "Agent namespace/name", "AgentTask namespace/name", or
// "AgentClass name": Agents first, then AgentTasks, then AgentClasses, each
// sorted, so the referrer a message names is stable from pass to pass.
func listReferrers(ctx context.Context, c client.Reader, name string, idx referrerIndexes) ([]string, error) {
	var out []string
	if idx.agent != "" {
		var agents kaalmv1beta1.AgentList
		if err := c.List(ctx, &agents, client.MatchingFields{idx.agent: name}); err != nil {
			return nil, err
		}
		var names []string
		for i := range agents.Items {
			names = append(names, "Agent "+agents.Items[i].Namespace+"/"+agents.Items[i].Name)
		}
		sort.Strings(names)
		out = append(out, names...)
	}
	if idx.task != "" {
		var tasks kaalmv1beta1.AgentTaskList
		if err := c.List(ctx, &tasks, client.MatchingFields{idx.task: name}); err != nil {
			return nil, err
		}
		var names []string
		for i := range tasks.Items {
			names = append(names, "AgentTask "+tasks.Items[i].Namespace+"/"+tasks.Items[i].Name)
		}
		sort.Strings(names)
		out = append(out, names...)
	}
	if idx.class != "" {
		var classes kaalmv1beta1.AgentClassList
		if err := c.List(ctx, &classes, client.MatchingFields{idx.class: name}); err != nil {
			return nil, err
		}
		var names []string
		for i := range classes.Items {
			names = append(names, "AgentClass "+classes.Items[i].Name)
		}
		sort.Strings(names)
		out = append(out, names...)
	}
	return out, nil
}

// deletionBlockedMessage names the first referrer and, when there are more,
// how many objects hold the delete in all.
func deletionBlockedMessage(refs []string) string {
	if len(refs) == 1 {
		return fmt.Sprintf("deletion blocked: %s still references it", refs[0])
	}
	return fmt.Sprintf("deletion blocked: %d objects still reference it, including %s", len(refs), refs[0])
}

// setDeletionBlocked sets Ready=False DeletionBlocked with a message naming
// a referrer and the count. It returns the message and whether the hold is
// new, so the caller sends the Warning event once its status write succeeds.
func setDeletionBlocked(conds *[]metav1.Condition, refs []string) (msg string, first bool) {
	msg = deletionBlockedMessage(refs)
	first = readyFalseIsNew(*conds, kaalmv1beta1.ReasonDeletionBlocked)
	apimeta.SetStatusCondition(conds, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: kaalmv1beta1.ReasonDeletionBlocked, Message: msg,
	})
	return msg, first
}

// holdDeletion makes a delete the finalizer holds visible: Ready=False with
// reason DeletionBlocked (setDeletionBlocked), and a Warning event when the
// hold first appears, emitted once the status write succeeds. It writes
// status only when a condition the pass set changed against before, the
// conditions as the pass read them, so a hold that waits on the same
// referrers costs nothing per pass, while one whose other conditions are
// stale (a Healthy set before the hold began) is written once. conds is
// obj's status.conditions.
func holdDeletion(
	ctx context.Context, c client.Client, rec record.EventRecorder,
	obj client.Object, conds *[]metav1.Condition, before []metav1.Condition, refs []string,
) error {
	msg, first := setDeletionBlocked(conds, refs)
	if equality.Semantic.DeepEqual(before, *conds) {
		return nil
	}
	if err := c.Status().Update(ctx, obj); err != nil {
		return err
	}
	// After the write, so a pass that read a stale cache and lost the write
	// to a conflict does not report the hold a second time.
	if first && rec != nil {
		rec.Event(obj, corev1.EventTypeWarning, kaalmv1beta1.ReasonDeletionBlocked, msg)
	}
	return nil
}
