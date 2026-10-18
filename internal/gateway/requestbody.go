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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The LLM proxy forwards the agent's request body as sent, apart from the
// model value, the names of the members it routes by, and the
// stream_options member a streaming OpenAI request gets. Providers act on
// fields the gateway never reads, so it must not change them: a decode into
// map[string]any and a re-encode would turn an integer above 2^53 into a
// float, reorder the members, and escape HTML characters. The gateway
// therefore reads the few top-level members it routes by from the bytes,
// rewrites the model value, and writes the routing member names exactly, so
// a provider that matches names exactly reads the members the gateway read.

// requestFields locates the top-level members of a request body the
// gateway routes and meters by.
type requestFields struct {
	// keyStart and keyEnd span each routing member's name token, quotes
	// included, indexed like routingNames; -1 when the member is absent.
	keyStart, keyEnd [len(routingNames)]int
	// modelStart and modelEnd span the model member's value; -1 when the
	// body has no model member.
	modelStart, modelEnd int
	// streamTrue is set when stream is the literal true.
	streamTrue bool
	// hasStreamOptions is set when a stream_options member is present,
	// whatever its value.
	hasStreamOptions bool
	// members counts the top-level members; closeBrace is the offset of
	// the object's closing brace.
	members, closeBrace int
}

// duplicateFieldError reports a top-level routing member the body names
// more than once. encoding/json matches struct field names case-insensitively
// and keeps the last match, so a case variant is another copy; a provider may
// keep either one, and then serve a model or a stream the gateway did not
// admit. name is the routing name, whatever spelling the body used.
type duplicateFieldError struct{ name string }

func (e *duplicateFieldError) Error() string {
	return fmt.Sprintf("request body has more than one %q field", e.name)
}

// routingNames are the top-level members the gateway routes and meters by,
// and routingKeys the same names as the JSON tokens the rewrite writes.
// The member constants index both.
var (
	routingNames = [...]string{"model", "stream", "stream_options"}
	routingKeys  = [...][]byte{[]byte(`"model"`), []byte(`"stream"`), []byte(`"stream_options"`)}
)

const (
	memberModel = iota
	memberStream
	memberStreamOptions
)

// jsonTrue is the JSON literal true.
var jsonTrue = []byte("true")

// errNotObject reports a JSON body that is not an object.
var errNotObject = errors.New("request body is not a JSON object")

// scanRequestFields reads the routing members of body, which must already
// pass json.Valid; the scan relies on that and checks nothing else. A null
// body has no members, as encoding/json reads it into a map. Any other
// non-object body is errNotObject, and a repeated model, stream, or
// stream_options member is a *duplicateFieldError. Member names are
// compared unescaped with jsonNameMatches, the rule encoding/json uses for
// struct fields, so a lone case variant is that member.
func scanRequestFields(body []byte) (requestFields, error) {
	f := requestFields{
		keyStart: [...]int{-1, -1, -1}, keyEnd: [...]int{-1, -1, -1},
		modelStart: -1, modelEnd: -1, closeBrace: -1,
	}
	i := skipJSONSpace(body, 0)
	if body[i] == 'n' {
		return f, nil
	}
	if body[i] != '{' {
		return f, errNotObject
	}
	i++
	for {
		i = skipJSONSpace(body, i)
		switch body[i] {
		case '}':
			f.closeBrace = i
			return f, nil
		case ',':
			i = skipJSONSpace(body, i+1)
		}
		keyStart := i
		keyEnd := skipJSONString(body, i)
		name := jsonStringValue(body[i:keyEnd])
		i = skipJSONSpace(body, keyEnd) + 1 // the colon
		valueStart := skipJSONSpace(body, i)
		i = skipJSONValue(body, valueStart)
		f.members++
		for idx, routing := range routingNames {
			if !jsonNameMatches(name, routing) {
				continue
			}
			if f.keyStart[idx] >= 0 {
				return f, &duplicateFieldError{name: routing}
			}
			f.keyStart[idx], f.keyEnd[idx] = keyStart, keyEnd
			switch idx {
			case memberModel:
				f.modelStart, f.modelEnd = valueStart, i
			case memberStream:
				f.streamTrue = bytes.Equal(body[valueStart:i], jsonTrue)
			case memberStreamOptions:
				f.hasStreamOptions = true
			}
			break // the routing names do not match one another
		}
	}
}

// model returns the model member's value when it is a JSON string, else "".
func (f requestFields) model(body []byte) string {
	if f.modelStart < 0 || body[f.modelStart] != '"' {
		return ""
	}
	return jsonStringValue(body[f.modelStart:f.modelEnd])
}

// streamOptionsMember is the member a streaming OpenAI request gets when it
// has no stream_options: without it the stream carries no usage.
const streamOptionsMember = `"stream_options":{"include_usage":true}`

// bodyEdit replaces body[start:end] with text; start == end inserts.
type bodyEdit struct {
	start, end int
	text       []byte
}

// rewriteRequestBody returns body with the model value replaced by model,
// each routing member's name written exactly, and, when addStreamOptions is
// set, the stream_options member appended. Every other byte is kept. A body
// without a model member gets no model edit.
func rewriteRequestBody(body []byte, f requestFields, model string, addStreamOptions bool) []byte {
	var edits [len(routingNames) + 2]bodyEdit
	n := 0
	for idx, key := range routingKeys {
		if f.keyStart[idx] >= 0 && !bytes.Equal(body[f.keyStart[idx]:f.keyEnd[idx]], key) {
			edits[n] = bodyEdit{f.keyStart[idx], f.keyEnd[idx], key}
			n++
		}
	}
	if f.modelStart >= 0 {
		encoded, _ := json.Marshal(model) // a string always encodes
		edits[n] = bodyEdit{f.modelStart, f.modelEnd, encoded}
		n++
	}
	if addStreamOptions {
		member := []byte("," + streamOptionsMember)
		if f.members == 0 {
			member = member[1:]
		}
		edits[n] = bodyEdit{f.closeBrace, f.closeBrace, member}
		n++
	}
	if n == 0 {
		return body
	}
	// Members come in any order, and the edits never overlap.
	for i := 1; i < n; i++ {
		for j := i; j > 0 && edits[j].start < edits[j-1].start; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
	size := len(body)
	for _, e := range edits[:n] {
		size += len(e.text) - (e.end - e.start)
	}
	out := make([]byte, 0, size)
	at := 0
	for _, e := range edits[:n] {
		out = append(out, body[at:e.start]...)
		out = append(out, e.text...)
		at = e.end
	}
	return append(out, body[at:]...)
}

// needsStreamOptions reports whether a request sent through a adds the
// stream_options member: the adapter's format needs it for stream usage,
// the request streams, and the agent set no stream_options of its own.
func needsStreamOptions(a providerAdapter, f requestFields) bool {
	return a.streamUsageOption() && f.streamTrue && !f.hasStreamOptions
}

// skipJSONSpace returns the offset of the first non-space byte at or after i.
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipJSONString returns the offset just past the string that opens at i.
func skipJSONString(b []byte, i int) int {
	for j := i + 1; ; j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
}

// skipJSONValue returns the offset just past the value that starts at i.
func skipJSONValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		depth := 0
		for j := i; ; j++ {
			switch b[j] {
			case '"':
				j = skipJSONString(b, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
	default: // a number or a literal
		j := i
		for j < len(b) {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j
			}
			j++
		}
		return j
	}
}

// jsonStringValue unquotes a JSON string token. Only a token with an escape
// or a non-ASCII byte goes through the decoder, which also replaces invalid
// UTF-8 the way encoding/json does everywhere else.
func jsonStringValue(token []byte) string {
	for _, c := range token[1 : len(token)-1] {
		if c == '\\' || c >= 0x80 {
			var s string
			_ = json.Unmarshal(token, &s)
			return s
		}
	}
	return string(token[1 : len(token)-1])
}

// jsonNameMatches reports whether encoding/json would fill a struct field
// named field from a member named name. Both are unescaped. encoding/json
// matches names case-insensitively with Unicode simple folding, so U+017F
// (long s) matches s and U+212A (Kelvin sign) matches k; strings.EqualFold
// is that rule exactly.
func jsonNameMatches(name, field string) bool {
	return strings.EqualFold(name, field)
}
