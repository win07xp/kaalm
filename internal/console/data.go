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
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// Data is the console's single data layer: every read the JSON API serves
// and every object the page templates render comes through here, from the
// Kubernetes API and nowhere else. The console holds no state of its own.
type Data struct {
	Reader client.Reader
}

// Namespaces lists all namespace names, sorted. Authorization filtering is
// the caller's job (the Gate); this is the candidate list.
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

// Fleet returns the namespace's agents as fleet rows, sorted by name.
func (d *Data) Fleet(ctx context.Context, namespace string) ([]FleetRow, error) {
	var list kaalmv1alpha1.AgentList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	rows := make([]FleetRow, 0, len(list.Items))
	for i := range list.Items {
		rows = append(rows, fleetRow(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// Agent returns one agent in detail; found is false when it does not exist.
func (d *Data) Agent(ctx context.Context, namespace, name string) (AgentDetail, bool, error) {
	var a kaalmv1alpha1.Agent
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return AgentDetail{}, false, nil
		}
		return AgentDetail{}, false, err
	}
	return agentDetail(&a), true, nil
}

// Tasks returns the namespace's task history, most recently started first
// (tasks with no start time sort last, by name).
func (d *Data) Tasks(ctx context.Context, namespace string) ([]TaskRow, error) {
	var list kaalmv1alpha1.AgentTaskList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	rows := make([]TaskRow, 0, len(list.Items))
	for i := range list.Items {
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
	return rows, nil
}

// Channels returns the namespace's channel health rows, sorted by name.
func (d *Data) Channels(ctx context.Context, namespace string) ([]ChannelRow, error) {
	var list kaalmv1alpha1.AgentChannelList
	if err := d.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	rows := make([]ChannelRow, 0, len(list.Items))
	for i := range list.Items {
		rows = append(rows, channelRow(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// Spend returns the namespace's budget rows across all providers, sorted by
// provider name. ModelProvider is cluster-scoped; only the rows belonging to
// the namespace being viewed are extracted.
func (d *Data) Spend(ctx context.Context, namespace string) ([]SpendRow, error) {
	var list kaalmv1alpha1.ModelProviderList
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
