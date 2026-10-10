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
)

// The LLM proxy forwards the agent's request body as sent, apart from the
// model value and the stream_options member a streaming OpenAI request
// gets. Providers act on fields the gateway never reads, so it must not
// change them: a decode into map[string]any and a re-encode would turn an
// integer above 2^53 into a float, reorder the members, and escape HTML
// characters. The gateway therefore reads the few top-level members it
// routes by from the bytes, and rewrites only the model value.

// requestFields locates the top-level members of a request body the
// gateway routes and meters by.
type requestFields struct {
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
// more than once. encoding/json keeps the last copy, but a provider may keep
// the first, and then serve a model or a stream the gateway did not admit.
type duplicateFieldError struct{ name string }

func (e *duplicateFieldError) Error() string {
	return fmt.Sprintf("request body has more than one %q field", e.name)
}

// jsonTrue is the JSON literal true.
var jsonTrue = []byte("true")

// errNotObject reports a JSON body that is not an object.
var errNotObject = errors.New("request body is not a JSON object")

// scanRequestFields reads the routing members of body, which must already
// pass json.Valid; the scan relies on that and checks nothing else. A null
// body has no members, as encoding/json reads it into a map. Any other
// non-object body is errNotObject, and a repeated model, stream, or
// stream_options member is a *duplicateFieldError. Member names are
// compared unescaped, as encoding/json compares them.
func scanRequestFields(body []byte) (requestFields, error) {
	f := requestFields{modelStart: -1, modelEnd: -1, closeBrace: -1}
	i := skipJSONSpace(body, 0)
	if body[i] == 'n' {
		return f, nil
	}
	if body[i] != '{' {
		return f, errNotObject
	}
	i++
	var seenStream, seenOptions bool
	for {
		i = skipJSONSpace(body, i)
		switch body[i] {
		case '}':
			f.closeBrace = i
			return f, nil
		case ',':
			i = skipJSONSpace(body, i+1)
		}
		keyEnd := skipJSONString(body, i)
		name := jsonStringValue(body[i:keyEnd])
		i = skipJSONSpace(body, keyEnd) + 1 // the colon
		valueStart := skipJSONSpace(body, i)
		i = skipJSONValue(body, valueStart)
		f.members++
		switch name {
		case "model":
			if f.modelStart >= 0 {
				return f, &duplicateFieldError{name: name}
			}
			f.modelStart, f.modelEnd = valueStart, i
		case "stream":
			if seenStream {
				return f, &duplicateFieldError{name: name}
			}
			seenStream = true
			f.streamTrue = bytes.Equal(body[valueStart:i], jsonTrue)
		case "stream_options":
			if seenOptions {
				return f, &duplicateFieldError{name: name}
			}
			seenOptions = true
			f.hasStreamOptions = true
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

// rewriteRequestBody returns body with the model value replaced by model
// and, when addStreamOptions is set, the stream_options member appended.
// Every other byte is kept. A body without a model member only gets the
// insertion.
func rewriteRequestBody(body []byte, f requestFields, model string, addStreamOptions bool) []byte {
	var encoded []byte
	if f.modelStart >= 0 {
		encoded, _ = json.Marshal(model) // a string always encodes
	}
	modelStart, modelEnd := f.modelStart, f.modelEnd
	if modelStart < 0 {
		if !addStreamOptions {
			return body
		}
		modelStart, modelEnd = f.closeBrace, f.closeBrace
	}
	size := len(body) - (modelEnd - modelStart) + len(encoded)
	if addStreamOptions {
		size += 1 + len(streamOptionsMember)
	}
	out := make([]byte, 0, size)
	out = append(out, body[:modelStart]...)
	out = append(out, encoded...)
	out = append(out, body[modelEnd:f.closeBrace]...)
	if addStreamOptions {
		if f.members > 0 {
			out = append(out, ',')
		}
		out = append(out, streamOptionsMember...)
	}
	return append(out, body[f.closeBrace:]...)
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
