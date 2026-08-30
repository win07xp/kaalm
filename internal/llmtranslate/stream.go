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
	"fmt"
	"time"
)

// Stream translates an SSE stream line by line. Feed takes one line as the
// relay read it (without the trailing newline) and returns the lines to
// write, each without a trailing newline; a blank line ends an event.
// Finish returns whatever closes the stream when the upstream ends without
// its own terminator.
type Stream interface {
	Feed(line []byte) [][]byte
	Finish() [][]byte
}

// NewStream returns the translator for one crossing, or nil when the formats
// match and the relay should copy lines.
func NewStream(from, to Format, model string) Stream {
	switch {
	case from == FormatOpenAI && to == FormatAnthropic:
		return &openAIToAnthropicStream{model: model, id: fmt.Sprintf("msg_%d", time.Now().UnixNano())}
	case from == FormatAnthropic && to == FormatOpenAI:
		return &anthropicToOpenAIStream{model: model, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
			created: time.Now().Unix()}
	}
	return nil
}

func sseEvent(name string, payload any) [][]byte {
	data, _ := json.Marshal(payload)
	return [][]byte{[]byte("event: " + name), append([]byte("data: "), data...), {}}
}

func sseData(payload any) [][]byte {
	data, _ := json.Marshal(payload)
	return [][]byte{append([]byte("data: "), data...), {}}
}

// ---- OpenAI chunks -> Anthropic events ----

type openAIToAnthropicStream struct {
	model, id    string
	started      bool
	blockOpen    bool
	blockIndex   int
	blockIsTool  bool
	toolIndexes  map[int]int // OpenAI tool_call index -> Anthropic block index
	finish       string
	deltaSent    bool
	stopped      bool
	outputTokens int64
	inputTokens  int64
}

type openAIChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (s *openAIToAnthropicStream) Feed(line []byte) [][]byte {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil // OpenAI streams carry only data lines; blanks and comments drop
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return s.Finish()
	}
	var chunk openAIChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	var out [][]byte
	if !s.started {
		s.started = true
		if chunk.Model != "" {
			s.model = chunk.Model
		}
		out = append(out, sseEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": s.id, "type": "message", "role": roleAssistant, "model": s.model, "content": []any{},
				"stop_reason": nil, stopSequence: nil,
				keyUsage: map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})...)
	}
	if chunk.Usage != nil {
		s.inputTokens, s.outputTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if !s.blockOpen || s.blockIsTool {
				out = append(out, s.openBlock(map[string]any{"type": typeText, typeText: ""}, false)...)
			}
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.blockIndex,
				"delta": map[string]any{"type": "text_delta", typeText: *choice.Delta.Content},
			})...)
		}
		for _, call := range choice.Delta.ToolCalls {
			if s.toolIndexes == nil {
				s.toolIndexes = map[int]int{}
			}
			if _, known := s.toolIndexes[call.Index]; !known {
				name := ""
				if call.Function != nil {
					name = call.Function.Name
				}
				out = append(out, s.openBlock(map[string]any{
					"type": typeToolUse, "id": call.ID, "name": name, keyInput: map[string]any{}}, true)...)
				s.toolIndexes[call.Index] = s.blockIndex
			}
			if call.Function != nil && call.Function.Arguments != "" {
				out = append(out, sseEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.toolIndexes[call.Index],
					"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
				})...)
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finish = *choice.FinishReason
		}
	}
	// The usage chunk (empty choices) follows the finish chunk: emit the
	// delta once both are known, or at Finish.
	if s.finish != "" && chunk.Usage != nil && !s.deltaSent {
		out = append(out, s.messageDelta()...)
	}
	return out
}

func (s *openAIToAnthropicStream) openBlock(block map[string]any, isTool bool) [][]byte {
	var out [][]byte
	if s.blockOpen {
		out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIndex})...)
		s.blockIndex++
	}
	s.blockOpen, s.blockIsTool = true, isTool
	return append(out, sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIndex, "content_block": block})...)
}

func (s *openAIToAnthropicStream) messageDelta() [][]byte {
	var out [][]byte
	if s.blockOpen {
		out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIndex})...)
		s.blockOpen = false
	}
	stop := finishToStop[s.finish]
	if stop == "" {
		stop = stopEndTurn
	}
	if s.finish == keyToolCalls || (s.toolIndexes != nil && s.finish == "stop") {
		stop = typeToolUse
	}
	s.deltaSent = true
	return append(out, sseEvent("message_delta", map[string]any{
		"type":   "message_delta",
		"delta":  map[string]any{"stop_reason": stop, stopSequence: nil},
		keyUsage: map[string]any{"input_tokens": s.inputTokens, "output_tokens": s.outputTokens},
	})...)
}

func (s *openAIToAnthropicStream) Finish() [][]byte {
	if s.stopped {
		return nil
	}
	s.stopped = true
	var out [][]byte
	if !s.started {
		return nil
	}
	if !s.deltaSent {
		out = append(out, s.messageDelta()...)
	}
	return append(out, sseEvent("message_stop", map[string]any{"type": "message_stop"})...)
}

// ---- Anthropic events -> OpenAI chunks ----

type anthropicToOpenAIStream struct {
	model, id string
	created   int64
	event     string
	toolIndex int
	blocks    map[int]int // Anthropic block index -> OpenAI tool_call index
	usage     struct{ in, out int64 }
	finished  bool
	done      bool
}

func (s *anthropicToOpenAIStream) chunk(delta map[string]any, finish any) [][]byte {
	return sseData(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", keyCreated: s.created, "model": s.model,
		keyChoices: []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

func (s *anthropicToOpenAIStream) Feed(line []byte) [][]byte {
	if name, ok := bytes.CutPrefix(line, []byte("event:")); ok {
		s.event = string(bytes.TrimSpace(name))
		return nil
	}
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
		return nil
	}
	typ, _ := ev["type"].(string)
	if typ == "" {
		typ = s.event
	}
	switch typ {
	case "message_start":
		msg, _ := ev["message"].(map[string]any)
		if m, ok := msg["model"].(string); ok && m != "" {
			s.model = m
		}
		if u, ok := msg[keyUsage].(map[string]any); ok {
			s.usage.in, _ = numberOf(u["input_tokens"])
		}
		return s.chunk(map[string]any{"role": roleAssistant, "content": ""}, nil)
	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		if block["type"] != typeToolUse {
			return nil
		}
		idx, _ := numberOf(ev["index"])
		if s.blocks == nil {
			s.blocks = map[int]int{}
		}
		s.blocks[int(idx)] = s.toolIndex
		out := s.chunk(map[string]any{keyToolCalls: []any{map[string]any{
			"index": s.toolIndex, "id": block["id"], "type": typeFunction,
			typeFunction: map[string]any{"name": block["name"], keyArguments: ""},
		}}}, nil)
		s.toolIndex++
		return out
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		switch delta["type"] {
		case "text_delta":
			return s.chunk(map[string]any{"content": delta[typeText]}, nil)
		case "input_json_delta":
			idx, _ := numberOf(ev["index"])
			return s.chunk(map[string]any{keyToolCalls: []any{map[string]any{
				"index": s.blocks[int(idx)], typeFunction: map[string]any{keyArguments: delta["partial_json"]},
			}}}, nil)
		}
	case "message_delta":
		delta, _ := ev["delta"].(map[string]any)
		if u, ok := ev[keyUsage].(map[string]any); ok {
			if n, ok := numberOf(u["output_tokens"]); ok {
				s.usage.out = n
			}
			if n, ok := numberOf(u["input_tokens"]); ok && n > 0 {
				s.usage.in = n
			}
		}
		stop, _ := delta["stop_reason"].(string)
		finish := stopToFinish[stop]
		if finish == "" {
			finish = "stop"
		}
		s.finished = true
		return s.chunk(map[string]any{}, finish)
	case "message_stop":
		return s.Finish()
	case "error":
		s.done = true
		return append(sseData(map[string]any{"error": ev["error"]}), []byte("data: [DONE]"), []byte{})
	}
	return nil
}

func (s *anthropicToOpenAIStream) Finish() [][]byte {
	if s.done {
		return nil
	}
	s.done = true
	var out [][]byte
	if !s.finished {
		out = append(out, s.chunk(map[string]any{}, "stop")...)
	}
	out = append(out, sseData(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", keyCreated: s.created, "model": s.model,
		keyChoices: []any{},
		keyUsage: map[string]any{"prompt_tokens": s.usage.in, "completion_tokens": s.usage.out,
			"total_tokens": s.usage.in + s.usage.out},
	})...)
	return append(out, []byte("data: [DONE]"), []byte{})
}
