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

package secretwatch

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// next reads one event from w, failing the test if none arrives in time.
func next(t *testing.T, w watch.Interface) watch.Event {
	t.Helper()
	select {
	case ev, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("the watch closed")
		}
		return ev
	case <-time.After(waitTimeout):
		t.Fatal("no event in time")
	}
	return watch.Event{}
}

// flush sends a bookmark on src and reads it from w. The forwarding
// goroutine records an event before it receives the next one, so once the
// bookmark arrives, every earlier event is recorded.
func flush(t *testing.T, src *watch.RaceFreeFakeWatcher, w watch.Interface) {
	t.Helper()
	src.Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}})
	if ev := next(t, w); ev.Type != watch.Bookmark {
		t.Fatalf("event = %s, want the flush bookmark", ev.Type)
	}
}

// seenUID returns the UID of the entry's seen Secret, or "" when none.
func seenUID(e *secretInformer) string {
	_, sec := e.currentSeen()
	if sec == nil {
		return ""
	}
	return string(sec.UID)
}

func TestConfirmedWatchDeletesTheDeadSecretAndDropsItsEvents(t *testing.T) {
	entry := &secretInformer{}
	entry.setSeen(hookAt("u1", "1"))
	gen, dead := entry.currentSeen()
	src := watch.NewRaceFreeFake()
	w := newConfirmedWatch(src, entry, gen, hookKey, dead)
	defer w.Stop()

	ev := next(t, w)
	tomb, ok := ev.Object.(*corev1.Secret)
	if ev.Type != watch.Deleted || !ok {
		t.Fatalf("first event = %s %T, want a Secret delete", ev.Type, ev.Object)
	}
	if tomb.Namespace != "team-a" || tomb.Name != "hook" || tomb.UID != "u1" || tomb.ResourceVersion != "" {
		t.Fatalf("delete = %+v, want team-a/hook, UID u1, no resourceVersion", tomb.ObjectMeta)
	}
	flush(t, src, w)
	if got := seenUID(entry); got != "" {
		t.Fatalf("seen after the delete = %q, want none", got)
	}

	// The replay and the real delete of the dead Secret are dropped.
	src.Add(hookAt("u1", "1"))
	src.Delete(hookAt("u1", "2"))
	// A new Secret under the name, a bookmark, a Secret under another name,
	// and an error pass through.
	src.Add(hookAt("u3", "3"))
	src.Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "4"}})
	other := secretObj("team-a", "other", "x")
	src.Add(other)
	src.Error(&metav1.Status{Status: metav1.StatusFailure, Message: "boom"})

	if ev := next(t, w); ev.Type != watch.Added || ev.Object.(*corev1.Secret).UID != "u3" {
		t.Fatalf("event = %s %+v, want the add of u3", ev.Type, ev.Object)
	}
	// The bookmark arrives after the add is recorded.
	if ev := next(t, w); ev.Type != watch.Bookmark {
		t.Fatalf("event = %s, want the bookmark", ev.Type)
	}
	if got := seenUID(entry); got != "u3" {
		t.Errorf("seen after the add = %q, want u3", got)
	}
	if ev := next(t, w); ev.Type != watch.Added || ev.Object != other {
		t.Fatalf("event = %s %+v, want the other Secret's add", ev.Type, ev.Object)
	}
	if ev := next(t, w); ev.Type != watch.Error {
		t.Fatalf("event = %s, want the error", ev.Type)
	}
	// The error arrives after the other Secret's add is handled.
	if got := seenUID(entry); got != "u3" {
		t.Errorf("seen after another Secret's add = %q, want u3", got)
	}
}

func TestConfirmedWatchRecordsWhatItForwards(t *testing.T) {
	entry := &secretInformer{}
	gen, _ := entry.currentSeen()
	src := watch.NewRaceFreeFake()
	w := newConfirmedWatch(src, entry, gen, hookKey, nil)
	defer w.Stop()

	src.Add(hookAt("u1", "1"))
	if ev := next(t, w); ev.Type != watch.Added {
		t.Fatalf("first event = %s, want the add (no synthetic delete without a dead Secret)", ev.Type)
	}
	src.Modify(hookAt("u1", "2"))
	next(t, w)
	flush(t, src, w)
	if _, sec := entry.currentSeen(); sec == nil || sec.ResourceVersion != "2" {
		t.Fatalf("seen = %+v, want resourceVersion 2", sec)
	}
	src.Delete(hookAt("u1", "3"))
	next(t, w)
	flush(t, src, w)
	if got := seenUID(entry); got != "" {
		t.Fatalf("seen after the delete = %q, want none", got)
	}

	// A later list starts a new generation: this watch no longer records.
	entry.setSeen(hookAt("u9", "9"))
	src.Add(hookAt("u2", "4"))
	next(t, w)
	flush(t, src, w)
	if got := seenUID(entry); got != "u9" {
		t.Fatalf("seen = %q, want the later list's u9", got)
	}
}

// Stop releases the forwarding goroutine even when nobody reads the events,
// and closes the result channel.
func TestConfirmedWatchStopsWithoutAReader(t *testing.T) {
	entry := &secretInformer{}
	entry.setSeen(hookAt("u1", "1"))
	gen, dead := entry.currentSeen()
	src := watch.NewRaceFreeFake()
	w := newConfirmedWatch(src, entry, gen, hookKey, dead)
	// The synthetic delete is waiting for a reader.
	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(waitTimeout):
		t.Fatal("Stop did not return")
	}
	if _, ok := <-w.ResultChan(); ok {
		t.Fatal("the result channel delivered after Stop")
	}
	if !src.IsStopped() {
		t.Error("the source watch is still running")
	}
	w.Stop()
}
