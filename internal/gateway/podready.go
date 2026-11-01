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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// PodReadyTracker follows agent Pods through the gateway's Pod watch, so a
// Pod wait connects when the Agent's Pod turns Ready instead of connecting
// to a Service with no endpoints. It keeps, per Agent, each Pod's name and
// whether it serves, and the waits in progress for that Agent; a Pod that
// starts serving wakes every wait for its Agent. The per-Agent cap on Pod
// waits is not counted here but in Server.podWaits (awaitPod): a wait that
// polls without the tracker (no Pod watch, or one not synced) registers no
// waiter here. An Agent's Pod is one the Agent controls (see agentPodKey);
// a Pod that only copies the Agent labels is ignored, because anyone who can
// create Pods in the namespace can copy labels.
type PodReadyTracker struct {
	// Synced reports whether the Pod watch has delivered its initial list.
	// nil or false means the tracker is not used and waits poll.
	Synced func() bool

	mu      sync.Mutex
	pods    map[agentKey]map[string]bool
	waiters map[agentKey]map[*podWaiter]struct{}
}

// agentKey names an Agent.
type agentKey struct{ namespace, name string }

// podWaiter is one Pod wait's registration. ch closes when a Pod for the
// Agent starts serving.
type podWaiter struct {
	tracker *PodReadyTracker
	key     agentKey
	ch      chan struct{}
}

// NewPodReadyTracker returns an empty tracker; set Synced and register
// Handler on the Pod informer.
func NewPodReadyTracker() *PodReadyTracker {
	return &PodReadyTracker{
		pods:    map[agentKey]map[string]bool{},
		waiters: map[agentKey]map[*podWaiter]struct{}{},
	}
}

// Handler is the Pod informer event handler that feeds the tracker.
func (t *PodReadyTracker) Handler() toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				t.observe(pod)
			}
		},
		UpdateFunc: func(_, obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				t.observe(pod)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				t.forget(pod)
			}
		},
	}
}

// agentPodKey returns the Agent a Pod belongs to, or false for a Pod that is
// not an Agent's (an AgentTask's Pod, or any other). An Agent's Pod carries
// the Agent labels and a controller owner reference to that Agent, as the
// controller sets both when it creates the Pod. Labels alone are not enough:
// anyone who can create Pods in the namespace can copy them. The reference
// is matched by API group, not group and version, so a Pod controlled
// through any version of the kaalm.io API counts.
func agentPodKey(pod *corev1.Pod) (agentKey, bool) {
	name := pod.Labels[kaalmv1beta1.LabelAgent]
	if pod.Labels[kaalmv1beta1.LabelWorkload] != kaalmv1beta1.WorkloadAgent || name == "" {
		return agentKey{}, false
	}
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != "Agent" || ref.Name != name {
		return agentKey{}, false
	}
	if gv, err := schema.ParseGroupVersion(ref.APIVersion); err != nil || gv.Group != kaalmv1beta1.GroupVersion.Group {
		return agentKey{}, false
	}
	return agentKey{namespace: pod.Namespace, name: name}, true
}

// podServing reports whether a Pod is one the Agent Service routes to and
// keeps routing to: Ready, not being deleted, and not finished.
func podServing(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil ||
		pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// observe records a Pod's state and, when it serves, wakes and drops every
// waiter for its Agent. Each channel closes once, under the lock.
func (t *PodReadyTracker) observe(pod *corev1.Pod) {
	key, ok := agentPodKey(pod)
	if !ok {
		return
	}
	serving := podServing(pod)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pods[key] == nil {
		t.pods[key] = map[string]bool{}
	}
	t.pods[key][pod.Name] = serving
	if !serving {
		return
	}
	for w := range t.waiters[key] {
		close(w.ch)
	}
	delete(t.waiters, key)
}

// forget drops a deleted Pod, and its Agent's entry with its last Pod.
func (t *PodReadyTracker) forget(pod *corev1.Pod) {
	key, ok := agentPodKey(pod)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pods[key], pod.Name)
	if len(t.pods[key]) == 0 {
		delete(t.pods, key)
	}
}

// usable reports whether waits can rely on the tracker: it exists and its
// Pod watch has synced.
func (t *PodReadyTracker) usable() bool {
	return t != nil && t.Synced != nil && t.Synced()
}

// Wait registers a waiter for the Agent namespace/name. The caller releases
// it when its wait ends.
func (t *PodReadyTracker) Wait(namespace, name string) *podWaiter {
	w := &podWaiter{tracker: t, key: agentKey{namespace: namespace, name: name}, ch: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiters[w.key] == nil {
		t.waiters[w.key] = map[*podWaiter]struct{}{}
	}
	t.waiters[w.key][w] = struct{}{}
	return w
}

// release drops the waiter. It is safe after the waiter fired.
func (w *podWaiter) release() {
	t := w.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiters[w.key], w)
	if len(t.waiters[w.key]) == 0 {
		delete(t.waiters, w.key)
	}
}

// ready reports whether any Pod of the Agent serves.
func (w *podWaiter) ready() bool {
	t := w.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, serving := range t.pods[w.key] {
		if serving {
			return true
		}
	}
	return false
}

// seen reports whether the tracker knows any Pod of the Agent.
func (w *podWaiter) seen() bool {
	t := w.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pods[w.key]) > 0
}
