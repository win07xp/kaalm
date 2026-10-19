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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/win07xp/kaalm/internal/eventrec"
)

// The helpers here bound what a condition message copies. Every
// spec-supplied or externally supplied value a message names is quoted with
// quotedEntry, a list of them is built with entryList, and free text copied
// whole is cut with cutDetail.
//
// The per-entry problem lists the AgentClass, ModelProvider, and ToolProvider
// reconcilers report on Ready, the lists in the advisory ModelProvider
// conditions, AgentClass SecurityBaseline, Agent ProvidersReady and
// BudgetExhausted, and the rule 38 list of granted tools a ToolProvider's
// catalog lacks each name up to maxListedEntries distinct entries, each spec
// value quoted up to maxQuotedEntryBytes, and count the rest in one message.
// In the worst case (%q writes a control byte as four bytes) one list is
// about 11 KiB, so several lists together can still pass the condition
// message limit; cutConditionMessage cuts the joined message there. The
// manager client (status_write_bound.go) cuts any message still past the
// limit, so no message ever fails a status write.
const (
	maxQuotedEntryBytes = 256
	maxListedEntries    = 10

	// maxDetailBytes bounds free text a condition message copies whole: an
	// API error, a kubelet message, the gateway's failure text. None has a
	// length limit, so each is cut at the Event note limit, as probe errors
	// and write rejections are.
	maxDetailBytes = eventrec.MaxNoteBytes

	// maxConditionMessageBytes is the condition schema's maxLength for
	// status.conditions[].message. A longer message fails the whole status
	// write and leaves Ready, observedGeneration, and every other condition
	// at their last values. Truncate counts bytes, which never undercounts
	// characters.
	maxConditionMessageBytes = 32768
)

// quotedEntry quotes s, or only its first maxQuotedEntryBytes bytes and its
// full length when it is longer. The cut steps back to a rune start so the
// quote never ends in a split rune.
func quotedEntry(s string) string {
	if len(s) <= maxQuotedEntryBytes {
		return fmt.Sprintf("%q", s)
	}
	cut := maxQuotedEntryBytes
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	if !utf8.RuneStart(s[cut]) {
		// Not valid UTF-8 here: %q escapes the bytes, so any cut reads right.
		cut = maxQuotedEntryBytes
	}
	return fmt.Sprintf("%q (first %d of %d bytes)", s[:cut], cut, len(s))
}

// entryList collects one list's problem messages: the first maxListedEntries
// distinct entries in the order the check finds them, and a count of the
// rest. The zero value is ready to use.
type entryList struct {
	seen   map[string]bool
	listed []string
	more   int
}

// add records the problem of the entry identified by key. A key already
// added is skipped, so a repeated entry is neither listed nor counted twice.
func (l *entryList) add(key, message string) {
	if l.seen[key] {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[key] = true
	if len(l.listed) < maxListedEntries {
		l.listed = append(l.listed, message)
		return
	}
	l.more++
}

// messages returns the listed messages, then one counting the rest: one
// when a single entry is not listed, or many (which holds a single %d) when
// several are.
func (l *entryList) messages(one, many string) []string {
	switch {
	case l.more == 1:
		return append(l.listed, one)
	case l.more > 1:
		return append(l.listed, fmt.Sprintf(many, l.more))
	}
	return l.listed
}

// joinProblems sorts msgs, joins them with "; ", and cuts the result with
// cutConditionMessage, so it always fits a condition message.
func joinProblems(msgs []string) string {
	sort.Strings(msgs)
	return cutConditionMessage(strings.Join(msgs, "; "))
}

// cutConditionMessage cuts s at maxConditionMessageBytes, so it always fits
// a condition message. A list joined in its own order, not sorted, ends here.
func cutConditionMessage(s string) string {
	return eventrec.Truncate(s, maxConditionMessageBytes)
}

// cutDetail cuts free text a condition message copies whole at
// maxDetailBytes.
func cutDetail(s string) string {
	return eventrec.Truncate(s, maxDetailBytes)
}
