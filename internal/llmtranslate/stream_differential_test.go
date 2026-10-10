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

package llmtranslate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// streamFixtures are the streams the differential tests replay: the streams
// of the stream tests and benchmarks, and the edge cases each translator
// handles (malformed lines, fields of the wrong type, events named only by
// the event line, escapes, feeds after the end).
func streamFixtures() map[string][]string {
	asStrings := func(lines [][]byte) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = string(l)
		}
		return out
	}
	return map[string][]string{
		"openai text":          asStrings(openAITextStream(3)),
		"openai tools":         asStrings(openAIToolStream(3)),
		"anthropic text":       asStrings(anthropicTextStream(3)),
		"anthropic tools":      asStrings(anthropicToolStream(3)),
		"anthropic max tokens": asStrings(append(anthropicTextStream(1)[:9], anthropicStreamEnd("max_tokens")...)),
		"openai mixed": {
			`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-5-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"index":0,"delta":{"content":"It is"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":" cold <b>&amp;</b> \u00e9\u2028 \"q\" \\ \t"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"<x>","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"after"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":7}}`,
			`data: [DONE]`,
			`data: {}`,
			`data: [DONE]`,
		},
		"openai odd lines": {
			`: keep-alive`,
			`data: not json`,
			`data: null`,
			`data: []`,
			`data:{"model":"m2","choices":[{"delta":{"tool_calls":[{"index":3}]},"finish_reason":"length"}]}`,
			`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1}}`,
			`data: {"choices":[{"delta":{"content":5}}]}`,
		},
		"openai ends without usage": {
			`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
		"openai tool then stop": {
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
		},
		"anthropic mixed": {
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":1}}}`,
			``,
			`event: ping`,
			`data: {"type":"ping"}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Cold <b>&amp;</b> \u00e9\u2028 \"q\" \\ \t \ud83d\ude00"}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"get_weather","input":{}}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":2.7,"content_block":{"type":"tool_use","id":7,"name":{"b":1,"a":[1.50,2e3]}}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Oslo\"}"}}`,
			`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":{"z":1,"a":null}}}`,
			`data: {"type":"content_block_delta","index":9,"delta":{"type":"input_json_delta"}}`,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9,"input_tokens":12}}`,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			`data: {"type":"message_stop"}`,
		},
		"anthropic odd lines": {
			`event: content_block_delta`,
			`data: {"index":0,"delta":{"type":"text_delta","text":"named by the event line"}}`,
			`data: null`,
			`data: [1,2]`,
			`data: "message_start"`,
			`data: {broken`,
			`data:`,
			`data: {"type":5,"delta":{"type":"text_delta","text":1.0}}`,
			`data: {"type":"content_block_delta","index":"0","delta":"text"}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":["a",{"y":true,"x":false}]}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta"}}`,
			`data: {"type":"content_block_start","content_block":"tool_use"}`,
			`data: {"type":"message_start","message":"m"}`,
			`data: {"type":"message_start","message":{"model":7,"usage":{"input_tokens":"3"}}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":-1,"input_tokens":0}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":7},"usage":[1]}`,
			`event: message_stop`,
			`data: {}`,
		},
		"anthropic error": {
			`event: message_start`, `data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`, ``,
			`event: error`, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded <now>"}}`, ``,
			`data: {"type":"message_stop"}`,
		},
		"anthropic ends without stop": {
			`event: message_start`, `data: {"type":"message_start","message":{"model":"m9"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`,
		},
	}
}

// translatorPair is a typed translator and the reference translator for
// the same crossing, with the same id, model, and creation time.
type translatorPair struct {
	name      string
	got, want Stream
}

func translatorPairs() []translatorPair {
	return []translatorPair{
		{"openai-to-anthropic",
			&openAIToAnthropicStream{model: "m0", id: "msg_1"},
			&refOpenAIToAnthropicStream{model: "m0", id: "msg_1"}},
		{"anthropic-to-openai",
			&anthropicToOpenAIStream{model: "m0", id: "chatcmpl-1", created: 1757600000},
			&refAnthropicToOpenAIStream{model: "m0", id: "chatcmpl-1", created: 1757600000}},
	}
}

// diffStream feeds lines to both translators of p, then finishes them, and
// reports the first output that is not byte-identical.
func diffStream(t *testing.T, p translatorPair, lines []string) {
	t.Helper()
	render := func(out [][]byte) string {
		parts := make([]string, len(out))
		for i, l := range out {
			parts[i] = strconv.Quote(string(l))
		}
		return strings.Join(parts, "\n")
	}
	for i, l := range lines {
		got, want := render(p.got.Feed([]byte(l))), render(p.want.Feed([]byte(l)))
		if got != want {
			t.Fatalf("%s: line %d %q:\ngot\n%s\nwant\n%s", p.name, i, l, got, want)
		}
	}
	if got, want := render(p.got.Finish()), render(p.want.Finish()); got != want {
		t.Fatalf("%s: Finish:\ngot\n%s\nwant\n%s", p.name, got, want)
	}
}

// TestStream_MatchesReference replays every fixture through both
// directions; each fixture also feeds the direction it was not written for,
// which exercises the dropped-line paths.
func TestStream_MatchesReference(t *testing.T) {
	for name, lines := range streamFixtures() {
		t.Run(name, func(t *testing.T) {
			for _, p := range translatorPairs() {
				diffStream(t, p, lines)
			}
		})
	}
}

// FuzzStream_MatchesReference feeds arbitrary lines to both directions and
// requires byte-identical output from the typed and reference translators.
//
// The Anthropic direction reads its per-token fields into a struct, which
// differs from a map in three ways no provider stream exercises: struct
// keys match case-insensitively, a repeated object key merges into the
// struct where a map keeps the last, and a number out of float64 range reads
// as zero where the map decode dropped the whole event. Inputs with any of
// these skip that direction.
func FuzzStream_MatchesReference(f *testing.F) {
	for _, lines := range streamFixtures() {
		f.Add(strings.Join(lines, "\n"))
	}
	f.Fuzz(func(t *testing.T, stream string) {
		lines := strings.Split(stream, "\n")
		for _, p := range translatorPairs() {
			if p.name == "anthropic-to-openai" && !mapLikeStream(lines) {
				continue
			}
			diffStream(t, p, lines)
		}
	})
}

// mapLikeStream reports whether every data line of lines decodes the same
// into a struct as into a map: see FuzzStream_MatchesReference.
func mapLikeStream(lines []string) bool {
	for _, l := range lines {
		if data, ok := strings.CutPrefix(l, "data:"); ok && !mapLikeJSON(strings.TrimSpace(data)) {
			return false
		}
	}
	return true
}

// mapLikeJSON reports whether data has no repeated or non-ASCII object
// keys, no key that matches an anthropicEvent field only when case is
// ignored, and no number out of float64 range. Invalid JSON counts as
// map-like: both decodes reject it.
func mapLikeJSON(data string) bool {
	fields := []string{"type", "index", "delta", "text", "partial_json", "stop_reason",
		"message", "content_block", "usage", "error"}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	type frame struct {
		object  bool
		wantKey bool
		keys    map[string]bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return true
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].wantKey = true
				}
				continue
			}
			key := tok.(string)
			if top.keys[key] {
				return false
			}
			top.keys[key] = true
			for i := 0; i < len(key); i++ {
				if key[i] >= 0x80 {
					return false
				}
			}
			for _, f := range fields {
				if key != f && strings.EqualFold(key, f) {
					return false
				}
			}
			top.wantKey = false
			continue
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, &frame{object: true, wantKey: true, keys: map[string]bool{}})
				continue
			case '[':
				stack = append(stack, &frame{})
				continue
			default:
				stack = stack[:len(stack)-1]
			}
		case json.Number:
			if _, err := strconv.ParseFloat(string(v), 64); err != nil {
				return false
			}
		}
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].wantKey = true
		}
	}
}

// TestMapLikeJSON pins the fuzz filter, so a filter that skips everything
// cannot hide a difference.
func TestMapLikeJSON(t *testing.T) {
	for data, want := range map[string]bool{
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`: true,
		`{"a":[{"b":1},{"b":2}],"c":{"d":{}}}`:                                              true,
		`not json`:                                                                          true,
		`{"type":"a","type":"b"}`:                                                           false,
		`{"delta":{"text":"a","text":"b"}}`:                                                 false,
		`{"Type":"message_start"}`:                                                          false,
		`{"delta":{"Partial_JSON":"x"}}`:                                                    false,
		`{"\u212a":1}`:                                                                      false,
		`{"index":1e400}`:                                                                   false,
		`[{"usage":{"x":1e309}}]`:                                                           false,
	} {
		if got := mapLikeJSON(data); got != want {
			t.Errorf("mapLikeJSON(%s) = %v, want %v", data, got, want)
		}
	}
}

// TestSSELines_SpareCapacity: a writer that appends the newline to a line
// must not write into memory another line shares.
func TestSSELines_SpareCapacity(t *testing.T) {
	var w sseLines
	out := w.event(nil, "message_stop", &anthropicMessageStop{Type: "message_stop"})
	out = w.data(out, &anthropicMessageStop{Type: "message_stop"})
	before := make([]string, len(out))
	for i, l := range out {
		before[i] = string(l)
	}
	for _, l := range out {
		_ = append(l, '\n')
	}
	for i, l := range out {
		if !bytes.Equal(l, []byte(before[i])) {
			t.Errorf("line %d changed to %q after appending to the lines", i, l)
		}
	}
}
