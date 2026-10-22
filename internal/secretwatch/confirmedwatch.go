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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// confirmedWatch is a watch started at the most recent version, after the
// informer's view of its Secret was confirmed with a GET. When that GET found
// the Secret gone, the watch first delivers a delete for it, because neither
// the informer's read nor the watch's opening replay carries the delete. It
// then forwards the real watch, dropping every event for the deleted
// Secret's UID: the replay and the real delete of a Secret removed after the
// watch's start version, which the one synthetic delete stands for. Every
// event it delivers for the Secret updates the entry's seen Secret.
//
// It does not use watch.Filter, whose goroutine blocks on an unread send
// after Stop.
type confirmedWatch struct {
	src    watch.Interface
	out    chan watch.Event
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
}

// newConfirmedWatch forwards src for the Secret at key. dead is the Secret
// the confirming GET found gone, or nil; gen is the list generation the
// watch records its events under.
func newConfirmedWatch(src watch.Interface, entry *secretInformer, gen uint64, key types.NamespacedName, dead *corev1.Secret) *confirmedWatch {
	w := &confirmedWatch{
		src:    src,
		out:    make(chan watch.Event),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	go w.run(entry, gen, key, dead)
	return w
}

// Stop stops the source watch and waits for the forwarding goroutine to end,
// so every event the reflector received is recorded before its next watch
// reads the seen Secret.
func (w *confirmedWatch) Stop() {
	w.once.Do(func() {
		close(w.done)
		w.src.Stop()
	})
	<-w.exited
}

// ResultChan returns the forwarded events; it closes when the source watch
// ends or Stop is called.
func (w *confirmedWatch) ResultChan() <-chan watch.Event { return w.out }

// send delivers ev unless the watch is stopped first.
func (w *confirmedWatch) send(ev watch.Event) bool {
	select {
	case w.out <- ev:
		return true
	case <-w.done:
		return false
	}
}

func (w *confirmedWatch) run(entry *secretInformer, gen uint64, key types.NamespacedName, dead *corev1.Secret) {
	defer close(w.exited)
	defer close(w.out)
	if dead != nil {
		// No resourceVersion, so the reflector keeps starting its watches
		// at the most recent version instead of resuming from one the API
		// server may have dropped. The change handler matches a delete on
		// namespace and name.
		tomb := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace, Name: key.Name, UID: dead.UID,
		}}
		if !w.send(watch.Event{Type: watch.Deleted, Object: tomb}) {
			return
		}
		entry.recordSeen(gen, nil)
	}
	for {
		var ev watch.Event
		var ok bool
		select {
		case <-w.done:
			return
		case ev, ok = <-w.src.ResultChan():
			if !ok {
				return
			}
		}
		sec, isSecret := ev.Object.(*corev1.Secret)
		forKey := isSecret && sec.Namespace == key.Namespace && sec.Name == key.Name &&
			(ev.Type == watch.Added || ev.Type == watch.Modified || ev.Type == watch.Deleted)
		if forKey && dead != nil && dead.UID != "" && sec.UID == dead.UID {
			continue
		}
		if !w.send(ev) {
			return
		}
		if !forKey {
			continue
		}
		if ev.Type == watch.Deleted {
			entry.recordSeen(gen, nil)
		} else {
			entry.recordSeen(gen, sec)
		}
	}
}
