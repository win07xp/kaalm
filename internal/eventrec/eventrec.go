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

// Package eventrec bounds the text Kaalm writes into Events and status
// messages. Several messages carry text from outside Kaalm (an agent's
// completion message, an API server rejection, a provider's error), and the
// API server rejects a write that exceeds a field's limit.
package eventrec

import (
	"fmt"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// MaxNoteBytes is the events.k8s.io/v1 limit on an Event's note. The API
// server rejects the whole Event above it.
const MaxNoteBytes = 1024

const truncatedSuffix = "... (truncated)"

// Truncate returns s cut to at most max bytes, ending in a truncation marker
// when it was cut. It never splits a UTF-8 character.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncatedSuffix)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedSuffix
}

// Bounded wraps rec so that every note fits MaxNoteBytes. A note with no
// args is passed on as written, so a message that contains a percent sign,
// such as a URL or an error, is never read as a format verb.
func Bounded(rec events.EventRecorder) events.EventRecorder {
	return bounded{rec: rec}
}

type bounded struct {
	rec events.EventRecorder
}

func (b bounded) Eventf(regarding, related runtime.Object, eventtype, reason, action, note string, args ...any) {
	if len(args) > 0 {
		note = fmt.Sprintf(note, args...)
	}
	b.rec.Eventf(regarding, related, eventtype, reason, action, "%s", Truncate(note, MaxNoteBytes))
}
