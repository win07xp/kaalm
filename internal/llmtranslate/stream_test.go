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
	"fmt"
	"testing"
)

// openAITextStream is an OpenAI chat stream of n content chunks, the finish
// chunk, the usage chunk, and [DONE], one line per element.
func openAITextStream(n int) [][]byte {
	lines := make([][]byte, 0, 2*n+6)
	for i := 0; i < n; i++ {
		lines = append(lines, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,`+
			`"model":"m1","choices":[{"index":0,"delta":{"content":"tok "},"finish_reason":null}]}`), nil)
	}
	return append(lines,
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), nil,
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",`+
			`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":200,"total_tokens":212}}`), nil,
		[]byte("data: [DONE]"), nil)
}

// openAIToolStream is an OpenAI chat stream that opens one tool call and
// streams its arguments in n chunks.
func openAIToolStream(n int) [][]byte {
	lines := [][]byte{
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",` +
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",` +
			`"function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`), nil,
	}
	for i := 0; i < n; i++ {
		lines = append(lines, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,`+
			`"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\":1}"}}]},`+
			`"finish_reason":null}]}`), nil)
	}
	return append(lines,
		[]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`), nil,
		[]byte(`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":200}}`), nil,
		[]byte("data: [DONE]"), nil)
}

// anthropicTextStream is an Anthropic messages stream with n text deltas,
// one line per element.
func anthropicTextStream(n int) [][]byte {
	lines := [][]byte{
		[]byte("event: message_start"),
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",` +
			`"model":"m1","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}`), nil,
		[]byte("event: content_block_start"),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`), nil,
	}
	for i := 0; i < n; i++ {
		lines = append(lines, []byte("event: content_block_delta"),
			[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tok "}}`), nil)
	}
	return append(lines, anthropicStreamEnd("end_turn")...)
}

// anthropicToolStream is an Anthropic messages stream that opens one
// tool_use block and streams its input in n deltas.
func anthropicToolStream(n int) [][]byte {
	lines := [][]byte{
		[]byte("event: message_start"),
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"m1","usage":{"input_tokens":12}}}`), nil,
		[]byte("event: content_block_start"),
		[]byte(`data: {"type":"content_block_start","index":0,` +
			`"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`), nil,
	}
	for i := 0; i < n; i++ {
		lines = append(lines, []byte("event: content_block_delta"),
			[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"k\":1}"}}`), nil)
	}
	return append(lines, anthropicStreamEnd("tool_use")...)
}

// anthropicStreamEnd closes block 0 and the message with stop.
func anthropicStreamEnd(stop string) [][]byte {
	return [][]byte{
		[]byte("event: content_block_stop"),
		[]byte(`data: {"type":"content_block_stop","index":0}`), nil,
		[]byte("event: message_delta"),
		[]byte(fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":200}}`, stop)), nil,
		[]byte("event: message_stop"),
		[]byte(`data: {"type":"message_stop"}`), nil,
	}
}

// BenchmarkStream translates a 200-delta stream of text or tool-call
// arguments in each direction, the translator alone.
func BenchmarkStream(b *testing.B) {
	for _, c := range []struct {
		name     string
		from, to Format
		lines    [][]byte
	}{
		{"openai-to-anthropic/text", FormatOpenAI, FormatAnthropic, openAITextStream(200)},
		{"openai-to-anthropic/tools", FormatOpenAI, FormatAnthropic, openAIToolStream(200)},
		{"anthropic-to-openai/text", FormatAnthropic, FormatOpenAI, anthropicTextStream(200)},
		{"anthropic-to-openai/tools", FormatAnthropic, FormatOpenAI, anthropicToolStream(200)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s := NewStream(c.from, c.to, "m1")
				for _, l := range c.lines {
					s.Feed(l)
				}
				s.Finish()
			}
		})
	}
}
