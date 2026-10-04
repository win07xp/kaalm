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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// DefaultAsyncOrphanPruneInterval is the delay between orphan-prune passes.
const DefaultAsyncOrphanPruneInterval = 10 * time.Minute

// asyncRecordTTL mirrors the gateway's fixed async record lifetime
// (internal/gateway asyncTTL). A record with no parseable expiry is pruned
// only once its creationTimestamp is older than twice this.
const asyncRecordTTL = time.Hour

// AsyncOrphanPruner deletes expired kaalm-async-* records whose channel no
// longer exists. The per-channel expiry prune runs only for live channels,
// and the finalizer sweep runs once; a gateway replica that had not yet
// observed Terminating can still create a record after that sweep, and this
// pass is what reaps it. Records of a channel that exists are left to that
// channel's own prune and finalizer.
type AsyncOrphanPruner struct {
	// Client reads from the manager's cache (ConfigMaps and AgentChannels
	// are both already informed) and issues the deletes.
	Client client.Client
	// OperatorNamespace holds the records.
	OperatorNamespace string
	// Interval is the delay between passes. Zero means
	// DefaultAsyncOrphanPruneInterval.
	Interval time.Duration

	now func() time.Time // tests pin the clock
}

// NeedLeaderElection runs the prune on the leader only: one deleter is enough.
func (p *AsyncOrphanPruner) NeedLeaderElection() bool { return true }

// Start runs a pass at once and then every Interval until ctx ends. A failed
// pass is logged and retried on the next tick; it never stops the manager.
func (p *AsyncOrphanPruner) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("async-orphan-prune")
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultAsyncOrphanPruneInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := p.pruneOnce(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "async orphan prune pass failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *AsyncOrphanPruner) pruneOnce(ctx context.Context) error {
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	var cms corev1.ConfigMapList
	if err := p.Client.List(ctx, &cms, client.InNamespace(p.OperatorNamespace)); err != nil {
		return err
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !strings.HasPrefix(cm.Name, "kaalm-async-") || !asyncRecordExpired(cm, now) {
			continue
		}
		chNS := cm.Labels[kaalmv1beta1.LabelChannelNamespace]
		chName := cm.Labels[kaalmv1beta1.LabelChannelName]
		if chNS != "" && chName != "" {
			err := p.Client.Get(ctx, types.NamespacedName{Namespace: chNS, Name: chName}, &kaalmv1beta1.AgentChannel{})
			if err == nil {
				continue // the channel's own prune owns this record
			}
			if !apierrors.IsNotFound(err) {
				return err
			}
		}
		if err := p.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// asyncRecordExpired reads the expiry annotation; without a parseable one it
// falls back to creationTimestamp plus twice the TTL, so a malformed record is
// reaped only when it is clearly past any lifetime the gateway gives. Both the
// per-channel prune (pruneAsyncConfigMaps) and the orphan pruner use it.
func asyncRecordExpired(cm *corev1.ConfigMap, now time.Time) bool {
	if expiresAt, err := time.Parse(time.RFC3339, cm.Annotations[kaalmv1beta1.AnnotationExpiresAt]); err == nil {
		return !expiresAt.After(now)
	}
	return !cm.CreationTimestamp.IsZero() && now.Sub(cm.CreationTimestamp.Time) > 2*asyncRecordTTL
}
