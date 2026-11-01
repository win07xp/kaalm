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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// agentPod is a Pod built the way the controller builds an Agent's Pod: the
// Agent labels and a controller owner reference to the Agent.
func agentPod(namespace, agent, name string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			Labels: map[string]string{
				kaalmv1beta1.LabelAgent:    agent,
				kaalmv1beta1.LabelWorkload: kaalmv1beta1.WorkloadAgent,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kaalmv1beta1.GroupVersion.String(),
				Kind:       "Agent",
				Name:       agent,
				Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

// syncedTracker is a tracker whose Pod watch reports its initial sync.
func syncedTracker() *PodReadyTracker {
	t := NewPodReadyTracker()
	t.Synced = func() bool { return true }
	return t
}

func fired(w *podWaiter) bool {
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

// A Pod serves only when it is Ready, not terminating, and not finished:
// a Ready Pod being deleted is about to leave the Service endpoints.
func TestPodReadyTracker_ServingNeedsReadyAndNotTerminating(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	w := tr.Wait("team-a", "sup")
	defer w.release()

	pod := agentPod("team-a", "sup", "sup-0", false)
	h.OnAdd(pod, false)
	if !w.seen() || w.ready() {
		t.Fatalf("not-Ready Pod: seen=%v ready=%v, want seen and not ready", w.seen(), w.ready())
	}
	if fired(w) {
		t.Fatal("waiter fired for a Pod that is not Ready")
	}

	readyPod := agentPod("team-a", "sup", "sup-0", true)
	h.OnUpdate(pod, readyPod)
	if !w.ready() || !fired(w) {
		t.Fatalf("Ready Pod: ready=%v fired=%v, want both", w.ready(), fired(w))
	}

	terminating := agentPod("team-a", "sup", "sup-0", true)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	h.OnUpdate(readyPod, terminating)
	if !w.seen() || w.ready() {
		t.Errorf("Ready terminating Pod: seen=%v ready=%v, want seen and not ready", w.seen(), w.ready())
	}

	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		finished := agentPod("team-a", "sup", "sup-0", true)
		finished.Status.Phase = phase
		h.OnUpdate(terminating, finished)
		if w.ready() {
			t.Errorf("Ready Pod in phase %s counted as serving", phase)
		}
	}
}

// AgentTask Pods and unlabeled Pods are not an Agent's Pods.
func TestPodReadyTracker_IgnoresOtherPods(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()

	task := agentPod("team-a", "sup", "task-0", true)
	task.Labels[kaalmv1beta1.LabelWorkload] = "task"
	h.OnAdd(task, false)
	h.OnAdd(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "other"}}, false)
	noAgent := agentPod("team-a", "", "blank-0", true)
	h.OnAdd(noAgent, false)

	if len(tr.pods) != 0 {
		t.Errorf("tracker holds %v, want no Pods", tr.pods)
	}
}

// A Pod is an Agent's only when the Agent controls it. Anyone who can create
// Pods in the namespace can copy the Agent labels, so a Pod with the labels
// but no controller reference of kind Agent is not recorded and wakes no
// waiter. The reference is matched by API group, so any version counts.
func TestPodReadyTracker_RequiresTheAgentControllerReference(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	w := tr.Wait("team-a", "sup")
	defer w.release()

	noOwner := agentPod("team-a", "sup", "copy-0", true)
	noOwner.OwnerReferences = nil
	notController := agentPod("team-a", "sup", "copy-1", true)
	notController.OwnerReferences[0].Controller = nil
	replicaSet := agentPod("team-a", "sup", "copy-2", true)
	replicaSet.OwnerReferences[0].APIVersion = "apps/v1"
	replicaSet.OwnerReferences[0].Kind = "ReplicaSet"
	otherName := agentPod("team-a", "sup", "copy-3", true)
	otherName.OwnerReferences[0].Name = "other"
	for _, pod := range []*corev1.Pod{noOwner, notController, replicaSet, otherName} {
		h.OnAdd(pod, false)
	}
	if len(tr.pods) != 0 {
		t.Errorf("tracker holds %v, want no Pods", tr.pods)
	}
	if w.seen() {
		t.Error("waiter saw a Pod that only copies the Agent labels")
	}
	if fired(w) {
		t.Error("waiter fired on a Pod that only copies the Agent labels")
	}

	otherVersion := agentPod("team-a", "sup", "sup-0", true)
	otherVersion.OwnerReferences[0].APIVersion = kaalmv1beta1.GroupVersion.Group + "/v1"
	h.OnAdd(otherVersion, false)
	if !w.seen() || !fired(w) {
		t.Errorf("Pod controlled through %s: seen %v, fired %v; want both",
			otherVersion.OwnerReferences[0].APIVersion, w.seen(), fired(w))
	}
}

// One Ready event wakes every waiter for that Agent, and only that Agent:
// the key includes the namespace.
func TestPodReadyTracker_WakesWaitersOnReady(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	a, b := tr.Wait("team-a", "sup"), tr.Wait("team-a", "sup")
	other := tr.Wait("team-b", "sup")
	defer a.release()
	defer b.release()
	defer other.release()

	h.OnAdd(agentPod("team-a", "sup", "sup-0", true), false)
	if !fired(a) || !fired(b) {
		t.Errorf("waiters for team-a/sup: fired %v, %v; want both", fired(a), fired(b))
	}
	if fired(other) {
		t.Error("waiter for team-b/sup fired on a team-a Pod")
	}
	// A second Ready event must not close the channels again.
	h.OnUpdate(agentPod("team-a", "sup", "sup-0", true), agentPod("team-a", "sup", "sup-0", true))

	h.OnAdd(agentPod("team-b", "sup", "sup-0", true), false)
	if !fired(other) {
		t.Error("waiter for team-b/sup did not fire on its own Pod")
	}
}

// During a replacement the old Pod is terminating and the new one is
// starting: the Agent has no serving Pod until the new one is Ready.
func TestPodReadyTracker_ReplacementWaitsForTheNewPod(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	old := agentPod("team-a", "sup", "sup-old", true)
	now := metav1.Now()
	old.DeletionTimestamp = &now
	h.OnAdd(old, false)
	fresh := agentPod("team-a", "sup", "sup-new", false)
	h.OnAdd(fresh, false)

	w := tr.Wait("team-a", "sup")
	defer w.release()
	if w.ready() || !w.seen() {
		t.Fatalf("replacement: seen=%v ready=%v, want seen and not ready", w.seen(), w.ready())
	}
	h.OnUpdate(fresh, agentPod("team-a", "sup", "sup-new", true))
	if !w.ready() || !fired(w) {
		t.Errorf("new Pod Ready: ready=%v fired=%v, want both", w.ready(), fired(w))
	}
}

// A Delete removes the Pod, whether it arrives as the Pod or as a tombstone
// after a relist, and the Agent's entry goes when its last Pod does.
func TestPodReadyTracker_DeleteAndTombstone(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	a := agentPod("team-a", "sup", "sup-0", true)
	b := agentPod("team-a", "sup", "sup-1", false)
	h.OnAdd(a, false)
	h.OnAdd(b, false)

	h.OnDelete(a)
	key := agentKey{namespace: "team-a", name: "sup"}
	if _, ok := tr.pods[key]["sup-0"]; ok {
		t.Error("deleted Pod still tracked")
	}
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "team-a/sup-1", Obj: b})
	if _, ok := tr.pods[key]; ok {
		t.Errorf("Agent entry kept after its last Pod's tombstone: %v", tr.pods[key])
	}
}

// release drops the waiter, so the per-Agent count only holds waits in
// progress.
func TestPodReadyTracker_ReleaseDropsTheWaiter(t *testing.T) {
	tr := syncedTracker()
	key := agentKey{namespace: "team-a", name: "sup"}
	a, b := tr.Wait("team-a", "sup"), tr.Wait("team-a", "sup")
	if got := len(tr.waiters[key]); got != 2 {
		t.Fatalf("waiters = %d, want 2", got)
	}
	a.release()
	b.release()
	if got := len(tr.waiters[key]); got != 0 {
		t.Errorf("waiters after release = %d, want 0", got)
	}
	// A release after the waiter fired is harmless.
	c := tr.Wait("team-a", "sup")
	tr.Handler().OnAdd(agentPod("team-a", "sup", "sup-0", true), false)
	c.release()
	if got := len(tr.waiters[key]); got != 0 {
		t.Errorf("waiters after a fired release = %d, want 0", got)
	}
}

// A nil tracker, or one whose watch has not synced, is not used.
func TestPodReadyTracker_UsableOnlyOnceSynced(t *testing.T) {
	var nilTracker *PodReadyTracker
	if nilTracker.usable() {
		t.Error("nil tracker reported usable")
	}
	tr := NewPodReadyTracker()
	if tr.usable() {
		t.Error("tracker with no Synced func reported usable")
	}
	tr.Synced = func() bool { return false }
	if tr.usable() {
		t.Error("unsynced tracker reported usable")
	}
	tr.Synced = func() bool { return true }
	if !tr.usable() {
		t.Error("synced tracker reported unusable")
	}
}
