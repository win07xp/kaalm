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
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// referrerIndexes names the field indexes that find the objects pinning a
// cluster-scoped resource against deletion. An index with no key is skipped:
// an AgentClass is pinned by workloads only.
type referrerIndexes struct {
	agent, task, class fieldIndex
}

// The referrer sets of the three resources a delete can hold.
var (
	modelProviderReferrers = referrerIndexes{
		agent: providerRefField, task: providerRefField, class: allowedProvidersField,
	}
	toolProviderReferrers = referrerIndexes{
		agent: toolProviderRefField, task: toolProviderRefField, class: allowedToolProvidersField,
	}
	agentClassReferrers = referrerIndexes{agent: agentClassRefField, task: agentClassRefField}
)

// deleteReferrers returns the objects that hold the delete of the resource
// called name. It lists from cached first. Only when that list is empty, the
// pass that would release the delete, does it list again from live, the API
// server: a referrer created moments before the delete may not be in the
// cache yet, and a released finalizer cannot be taken back. A nil live reads
// from cached, which suits a client with no cache.
func deleteReferrers(ctx context.Context, cached, live client.Reader, name string, idx referrerIndexes) ([]string, error) {
	refs, err := listReferrers(ctx, cached, name, idx, false)
	if err != nil || len(refs) > 0 {
		return refs, err
	}
	if live == nil {
		live = cached
	}
	refs, err = listReferrers(ctx, live, name, idx, true)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		log.FromContext(ctx).V(1).Info("delete held by referrers the cache does not hold yet", "referrers", len(refs))
	}
	return refs, nil
}

// listReferrers returns every Agent, AgentTask, and AgentClass whose index
// matches name, as "Agent namespace/name", "AgentTask namespace/name", or
// "AgentClass name": Agents first, then AgentTasks, then AgentClasses, each
// sorted, so the referrer a message names is stable from pass to pass. With
// live false it reads c's field indexes. With live true it lists each kind
// in full and applies the index extractor, for a reader with no indexes.
func listReferrers(ctx context.Context, c client.Reader, name string, idx referrerIndexes, live bool) ([]string, error) {
	kinds := []struct {
		f     fieldIndex
		list  client.ObjectList
		label string
	}{
		{idx.agent, &kaalmv1beta1.AgentList{}, "Agent"},
		{idx.task, &kaalmv1beta1.AgentTaskList{}, "AgentTask"},
		{idx.class, &kaalmv1beta1.AgentClassList{}, "AgentClass"},
	}
	var out []string
	for _, k := range kinds {
		if k.f.key == "" {
			continue
		}
		var opts []client.ListOption
		if !live {
			opts = append(opts, client.MatchingFields{k.f.key: name})
		}
		if err := c.List(ctx, k.list, opts...); err != nil {
			return nil, err
		}
		items, err := apimeta.ExtractList(k.list)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok || (live && !slices.Contains(k.f.extract(obj), name)) {
				continue
			}
			ref := obj.GetName()
			if ns := obj.GetNamespace(); ns != "" {
				ref = ns + "/" + ref
			}
			names = append(names, k.label+" "+ref)
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
	ctx context.Context, c client.Client, rec events.EventRecorder,
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
		rec.Eventf(obj, nil, corev1.EventTypeWarning, kaalmv1beta1.ReasonDeletionBlocked, kaalmv1beta1.ActionDelete, "%s", msg)
	}
	return nil
}
