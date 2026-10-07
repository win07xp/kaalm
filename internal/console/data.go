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

package console

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// Data is the console's single data layer: every read the JSON API serves
// and every object the page templates render comes through here, from the
// Kubernetes API and nowhere else. The console holds no state of its own.
type Data struct {
	Reader client.Reader
}

// Namespaces lists all namespace names, sorted. Authorization filtering is
// the caller's job (the AccessChecker); this is the candidate list.
func (d *Data) Namespaces(ctx context.Context) ([]string, error) {
	var list corev1.NamespaceList
	if err := d.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names, nil
}

// Fleet returns the newest limit agents in the namespace as fleet rows,
// sorted by name, and the namespace's total agent count.
func (d *Data) Fleet(ctx context.Context, namespace string, limit int) ([]FleetRow, int, error) {
	var list kaalmv1beta1.AgentList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, 0, err
	}
	keep := newest(len(list.Items), limit, func(i int) *metav1.ObjectMeta { return &list.Items[i].ObjectMeta })
	rows := make([]FleetRow, 0, len(keep))
	for _, i := range keep {
		rows = append(rows, fleetRow(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, len(list.Items), nil
}

// Agent returns one agent in detail; found is false when it does not exist.
func (d *Data) Agent(ctx context.Context, namespace, name string) (AgentDetail, bool, error) {
	var a kaalmv1beta1.Agent
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return AgentDetail{}, false, nil
		}
		return AgentDetail{}, false, err
	}
	return agentDetail(&a), true, nil
}

// Tasks returns the newest limit tasks in the namespace, most recently
// started first (tasks with no start time sort last, by name), and the
// namespace's total task count.
func (d *Data) Tasks(ctx context.Context, namespace string, limit int) ([]TaskRow, int, error) {
	var list kaalmv1beta1.AgentTaskList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, 0, err
	}
	keep := newest(len(list.Items), limit, func(i int) *metav1.ObjectMeta { return &list.Items[i].ObjectMeta })
	rows := make([]TaskRow, 0, len(keep))
	for _, i := range keep {
		rows = append(rows, taskRow(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.StartTime != nil && b.StartTime != nil && !a.StartTime.Equal(*b.StartTime):
			return a.StartTime.After(*b.StartTime)
		case (a.StartTime != nil) != (b.StartTime != nil):
			return a.StartTime != nil
		default:
			return a.Name < b.Name
		}
	})
	return rows, len(list.Items), nil
}

// Channels returns the newest limit channels in the namespace as health
// rows, sorted by name, and the namespace's total channel count.
func (d *Data) Channels(ctx context.Context, namespace string, limit int) ([]ChannelRow, int, error) {
	var list kaalmv1beta1.AgentChannelList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, 0, err
	}
	keep := newest(len(list.Items), limit, func(i int) *metav1.ObjectMeta { return &list.Items[i].ObjectMeta })
	rows := make([]ChannelRow, 0, len(keep))
	for _, i := range keep {
		rows = append(rows, channelRow(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, len(list.Items), nil
}

// newest returns the indexes of the limit most recently created of n
// objects (ties broken by name), so a truncated list keeps the newest rows
// whatever order the caller then displays them in. The list-route limit
// (defaultListLimit, maxListLimit) is applied here, after the list read.
func newest(n, limit int, meta func(i int) *metav1.ObjectMeta) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	if n <= limit {
		return idx
	}
	sort.Slice(idx, func(a, b int) bool {
		ma, mb := meta(idx[a]), meta(idx[b])
		if !ma.CreationTimestamp.Equal(&mb.CreationTimestamp) {
			return mb.CreationTimestamp.Before(&ma.CreationTimestamp)
		}
		return ma.Name < mb.Name
	})
	return idx[:limit]
}

// Spend returns the namespace's budget rows across all providers, sorted by
// provider name. ModelProvider is cluster-scoped; only the rows belonging to
// the namespace being viewed are extracted.
func (d *Data) Spend(ctx context.Context, namespace string) ([]SpendRow, error) {
	var list kaalmv1beta1.ModelProviderList
	if err := d.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]SpendRow, 0)
	for i := range list.Items {
		rows = append(rows, spendRows(&list.Items[i], namespace)...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Provider < rows[j].Provider })
	return rows, nil
}
