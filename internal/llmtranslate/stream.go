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

// sseLines renders SSE lines for one stream. Payloads are typed structs
// whose fields are in key order, so each renders exactly as the same keys in
// a map would. Not safe for concurrent use, like the Stream that owns it.
type sseLines struct {
	buf bytes.Buffer
	enc *json.Encoder
}

// event appends an event line, the payload's data line, and the blank line
// that ends the event. Each line is a new slice with one spare byte of
// capacity, so a writer that appends the newline does not copy it again.
func (w *sseLines) event(out [][]byte, name string, payload any) [][]byte {
	b := make([]byte, 0, len("event: ")+len(name)+1)
	return w.data(append(out, append(append(b, "event: "...), name...)), payload)
}

// data appends the payload's data line and the blank line that ends the
// event.
func (w *sseLines) data(out [][]byte, payload any) [][]byte {
	if w.enc == nil {
		w.enc = json.NewEncoder(&w.buf)
	}
	w.buf.Reset()
	if err := w.enc.Encode(payload); err != nil {
		w.buf.Reset()
	}
	// Encode ends the value with a newline, which the line does not carry.
	payloadJSON := bytes.TrimSuffix(w.buf.Bytes(), []byte("\n"))
	b := make([]byte, 0, len("data: ")+len(payloadJSON)+1)
	return append(out, append(append(b, "data: "...), payloadJSON...), []byte{})
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
	lines        sseLines
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

// The Anthropic events this direction writes. Fields are in key order.
type (
	anthropicUsage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	anthropicMessageStart struct {
		Message struct {
			Content      [0]struct{}    `json:"content"`
			ID           string         `json:"id"`
			Model        string         `json:"model"`
			Role         string         `json:"role"`
			StopReason   *string        `json:"stop_reason"`
			StopSequence *string        `json:"stop_sequence"`
			Type         string         `json:"type"`
			Usage        anthropicUsage `json:"usage"`
		} `json:"message"`
		Type string `json:"type"`
	}
	anthropicTextBlock struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	anthropicToolUseBlock struct {
		ID    string   `json:"id"`
		Input struct{} `json:"input"`
		Name  string   `json:"name"`
		Type  string   `json:"type"`
	}
	anthropicBlockStart struct {
		ContentBlock any    `json:"content_block"`
		Index        int    `json:"index"`
		Type         string `json:"type"`
	}
	anthropicTextDelta struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	anthropicInputJSONDelta struct {
		PartialJSON string `json:"partial_json"`
		Type        string `json:"type"`
	}
	anthropicBlockDelta struct {
		Delta any    `json:"delta"`
		Index int    `json:"index"`
		Type  string `json:"type"`
	}
	anthropicBlockStop struct {
		Index int    `json:"index"`
		Type  string `json:"type"`
	}
	anthropicMessageDelta struct {
		Delta struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Type  string         `json:"type"`
		Usage anthropicUsage `json:"usage"`
	}
	anthropicMessageStop struct {
		Type string `json:"type"`
	}
)

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
		var start anthropicMessageStart
		start.Type = "message_start"
		start.Message.ID, start.Message.Type, start.Message.Role, start.Message.Model =
			s.id, "message", roleAssistant, s.model
		out = s.lines.event(out, "message_start", &start)
	}
	if chunk.Usage != nil {
		s.inputTokens, s.outputTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if !s.blockOpen || s.blockIsTool {
				out = s.openBlock(out, &anthropicTextBlock{Type: typeText}, false)
			}
			out = s.lines.event(out, "content_block_delta", &anthropicBlockDelta{
				Type: "content_block_delta", Index: s.blockIndex,
				Delta: &anthropicTextDelta{Type: "text_delta", Text: *choice.Delta.Content},
			})
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
				out = s.openBlock(out, &anthropicToolUseBlock{Type: typeToolUse, ID: call.ID, Name: name}, true)
				s.toolIndexes[call.Index] = s.blockIndex
			}
			if call.Function != nil && call.Function.Arguments != "" {
				out = s.lines.event(out, "content_block_delta", &anthropicBlockDelta{
					Type: "content_block_delta", Index: s.toolIndexes[call.Index],
					Delta: &anthropicInputJSONDelta{Type: "input_json_delta", PartialJSON: call.Function.Arguments},
				})
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finish = *choice.FinishReason
		}
	}
	// The usage chunk (empty choices) follows the finish chunk: emit the
	// delta once both are known, or at Finish.
	if s.finish != "" && chunk.Usage != nil && !s.deltaSent {
		out = s.messageDelta(out)
	}
	return out
}

func (s *openAIToAnthropicStream) openBlock(out [][]byte, block any, isTool bool) [][]byte {
	if s.blockOpen {
		out = s.lines.event(out, "content_block_stop", &anthropicBlockStop{Type: "content_block_stop", Index: s.blockIndex})
		s.blockIndex++
	}
	s.blockOpen, s.blockIsTool = true, isTool
	return s.lines.event(out, "content_block_start", &anthropicBlockStart{
		Type: "content_block_start", Index: s.blockIndex, ContentBlock: block})
}

func (s *openAIToAnthropicStream) messageDelta(out [][]byte) [][]byte {
	if s.blockOpen {
		out = s.lines.event(out, "content_block_stop", &anthropicBlockStop{Type: "content_block_stop", Index: s.blockIndex})
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
	delta := anthropicMessageDelta{Type: "message_delta",
		Usage: anthropicUsage{InputTokens: s.inputTokens, OutputTokens: s.outputTokens}}
	delta.Delta.StopReason = stop
	return s.lines.event(out, "message_delta", &delta)
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
		out = s.messageDelta(out)
	}
	return s.lines.event(out, "message_stop", &anthropicMessageStop{Type: "message_stop"})
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
	lines     sseLines
}

// anthropicEvent is one Anthropic stream event. The per-token fields are
// typed. The fields of the once-per-stream events stay generic, read as the
// map lookups below, and the pass-through values (text, partial_json, error)
// keep whatever JSON type the upstream sent.
type anthropicEvent struct {
	Type  string  `json:"type"`
	Index float64 `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        any    `json:"text"`
		PartialJSON any    `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message      any `json:"message"`
	ContentBlock any `json:"content_block"`
	Usage        any `json:"usage"`
	Error        any `json:"error"`
}

// The OpenAI chunks this direction writes. Fields are in key order.
type (
	openAIChoice struct {
		Delta        any `json:"delta"`
		FinishReason any `json:"finish_reason"`
		Index        int `json:"index"`
	}
	openAIStreamChunk struct {
		Choices [1]openAIChoice `json:"choices"`
		Created int64           `json:"created"`
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Object  string          `json:"object"`
	}
	openAIUsageChunk struct {
		Choices [0]struct{} `json:"choices"`
		Created int64       `json:"created"`
		ID      string      `json:"id"`
		Model   string      `json:"model"`
		Object  string      `json:"object"`
		Usage   struct {
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	openAIRoleDelta struct {
		Content string `json:"content"`
		Role    string `json:"role"`
	}
	openAIContentDelta struct {
		Content any `json:"content"`
	}
	openAIToolStart struct {
		Function struct {
			Arguments string `json:"arguments"`
			Name      any    `json:"name"`
		} `json:"function"`
		ID    any    `json:"id"`
		Index int    `json:"index"`
		Type  string `json:"type"`
	}
	openAIToolArguments struct {
		Function struct {
			Arguments any `json:"arguments"`
		} `json:"function"`
		Index int `json:"index"`
	}
	openAIToolCallsDelta[T any] struct {
		ToolCalls [1]T `json:"tool_calls"`
	}
	openAIErrorChunk struct {
		Error any `json:"error"`
	}
)

func (s *anthropicToOpenAIStream) chunk(delta, finish any) [][]byte {
	c := openAIStreamChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model}
	c.Choices[0] = openAIChoice{Delta: delta, FinishReason: finish}
	return s.lines.data(nil, &c)
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
	data = bytes.TrimSpace(data)
	var ev anthropicEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		// A field of the wrong JSON type reads as absent, as a failed map
		// lookup did; anything that is not an object, or not JSON, drops.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) || data[0] != '{' {
			return nil
		}
	}
	typ := ev.Type
	if typ == "" {
		typ = s.event
	}
	switch typ {
	case "message_start":
		msg, _ := ev.Message.(map[string]any)
		if m, ok := msg["model"].(string); ok && m != "" {
			s.model = m
		}
		if u, ok := msg[keyUsage].(map[string]any); ok {
			s.usage.in, _ = numberOf(u["input_tokens"])
		}
		return s.chunk(&openAIRoleDelta{Role: roleAssistant}, nil)
	case "content_block_start":
		block, _ := ev.ContentBlock.(map[string]any)
		if block["type"] != typeToolUse {
			return nil
		}
		if s.blocks == nil {
			s.blocks = map[int]int{}
		}
		s.blocks[int(int64(ev.Index))] = s.toolIndex
		var call openAIToolCallsDelta[openAIToolStart]
		call.ToolCalls[0].Index, call.ToolCalls[0].ID, call.ToolCalls[0].Type = s.toolIndex, block["id"], typeFunction
		call.ToolCalls[0].Function.Name = block["name"]
		out := s.chunk(&call, nil)
		s.toolIndex++
		return out
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return s.chunk(&openAIContentDelta{Content: ev.Delta.Text}, nil)
		case "input_json_delta":
			var call openAIToolCallsDelta[openAIToolArguments]
			call.ToolCalls[0].Index = s.blocks[int(int64(ev.Index))]
			call.ToolCalls[0].Function.Arguments = ev.Delta.PartialJSON
			return s.chunk(&call, nil)
		}
	case "message_delta":
		if u, ok := ev.Usage.(map[string]any); ok {
			if n, ok := numberOf(u["output_tokens"]); ok {
				s.usage.out = n
			}
			if n, ok := numberOf(u["input_tokens"]); ok && n > 0 {
				s.usage.in = n
			}
		}
		finish := stopToFinish[ev.Delta.StopReason]
		if finish == "" {
			finish = "stop"
		}
		s.finished = true
		return s.chunk(&struct{}{}, finish)
	case "message_stop":
		return s.Finish()
	case "error":
		s.done = true
		return append(s.lines.data(nil, &openAIErrorChunk{Error: ev.Error}), []byte("data: [DONE]"), []byte{})
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
		out = s.chunk(&struct{}{}, "stop")
	}
	u := openAIUsageChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model}
	u.Usage.PromptTokens, u.Usage.CompletionTokens, u.Usage.TotalTokens = s.usage.in, s.usage.out, s.usage.in+s.usage.out
	out = s.lines.data(out, &u)
	return append(out, []byte("data: [DONE]"), []byte{})
}
