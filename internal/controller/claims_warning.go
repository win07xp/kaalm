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
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// claimsWarnings remembers, per object UID, the ResourceClaimsIgnored message
// last emitted (rule 53), so the Warning fires when an object's container
// resource claims first appear or change and not on every pass. The finding
// is advisory and has no condition, so its rising edge lives in memory: a
// controller restart or leader change emits it once more for each object
// that still sets claims. The zero value is ready to use.
type claimsWarnings struct {
	mu   sync.Mutex
	sent map[types.UID]string
}

// note emits the Warning on obj when claims is non-empty and differs from
// what was last emitted for it, and forgets obj when claims is empty. field
// names the resources block that holds the claims, such as "spec.resources".
func (w *claimsWarnings) note(rec record.EventRecorder, obj client.Object, field string, claims []corev1.ResourceClaim) {
	if rec == nil {
		return
	}
	uid := obj.GetUID()
	if len(claims) == 0 {
		w.forget(uid)
		return
	}
	names := make([]string, 0, len(claims))
	for _, c := range claims {
		if c.Request != "" {
			names = append(names, c.Name+"/"+c.Request)
		} else {
			names = append(names, c.Name)
		}
	}
	msg := fmt.Sprintf("%s.claims is ignored: the controller removes container resource claims (%s) "+
		"from workload Pods, because it never sets pod.spec.resourceClaims", field, strings.Join(names, ", "))
	w.mu.Lock()
	if w.sent[uid] == msg {
		w.mu.Unlock()
		return
	}
	if w.sent == nil {
		w.sent = map[types.UID]string{}
	}
	w.sent[uid] = msg
	w.mu.Unlock()
	rec.Event(obj, corev1.EventTypeWarning, kaalmv1beta1.ReasonResourceClaimsIgnored, msg)
}

// forget drops what was emitted for uid, so the next note with claims emits.
func (w *claimsWarnings) forget(uid types.UID) {
	w.mu.Lock()
	delete(w.sent, uid)
	w.mu.Unlock()
}
