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
	"encoding/json"
	"fmt"
	"time"
)

// Stop reasons, both ways. tool_calls beats stop when tool calls are present.
var stopToFinish = map[string]string{
	stopEndTurn: "stop", "max_tokens": "length", typeToolUse: keyToolCalls,
	stopSequence: "stop", "refusal": "content_filter", "pause_turn": "stop",
}

var finishToStop = map[string]string{
	"stop": stopEndTurn, "length": "max_tokens", keyToolCalls: typeToolUse, "content_filter": "refusal",
}

// Response rewrites a non-streaming success body from one format to the
// other. The model in the output is what the upstream reported.
func Response(from, to Format, body []byte) ([]byte, error) {
	switch {
	case from == to:
		return body, nil
	case from == FormatAnthropic && to == FormatOpenAI:
		return anthropicToOpenAIResponse(body)
	case from == FormatOpenAI && to == FormatAnthropic:
		return openAIToAnthropicResponse(body)
	}
	return nil, fmt.Errorf("no translation from %s to %s", from, to)
}

type anthropicResponse struct {
	ID         string           `json:"id"`
	Model      string           `json:"model"`
	Content    []map[string]any `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func anthropicToOpenAIResponse(body []byte) ([]byte, error) {
	var in anthropicResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	message := map[string]any{"role": roleAssistant, "content": nil}
	var text string
	var toolCalls []any
	for _, block := range in.Content {
		switch block["type"] {
		case typeText:
			if t, ok := block[typeText].(string); ok {
				if text != "" {
					text += "\n"
				}
				text += t
			}
		case typeToolUse:
			args, _ := json.Marshal(block[keyInput])
			toolCalls = append(toolCalls, map[string]any{
				"id": block["id"], "type": typeFunction,
				typeFunction: map[string]any{"name": block["name"], keyArguments: string(args)},
			})
		}
	}
	if text != "" {
		message["content"] = text
	}
	if len(toolCalls) > 0 {
		message[keyToolCalls] = toolCalls
	}
	finish := stopToFinish[in.StopReason]
	if finish == "" {
		finish = "stop"
	}
	prompt := in.Usage.InputTokens + in.Usage.CacheReadInputTokens + in.Usage.CacheCreationInputTokens
	out := map[string]any{
		"id": in.ID, "object": "chat.completion", keyCreated: time.Now().Unix(), "model": in.Model,
		keyChoices: []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		keyUsage: map[string]any{
			"prompt_tokens": prompt, "completion_tokens": in.Usage.OutputTokens,
			"total_tokens": prompt + in.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   any `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func openAIToAnthropicResponse(body []byte) ([]byte, error) {
	var in openAIResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	content := []any{}
	stop := stopEndTurn
	if len(in.Choices) > 0 {
		choice := in.Choices[0]
		if text := contentText(choice.Message.Content); text != "" {
			content = append(content, map[string]any{"type": typeText, typeText: text})
		}
		for _, call := range choice.Message.ToolCalls {
			var input any = map[string]any{}
			if call.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					input = map[string]any{"_arguments": call.Function.Arguments}
				}
			}
			content = append(content, map[string]any{
				"type": typeToolUse, "id": call.ID, "name": call.Function.Name, keyInput: input,
			})
		}
		if s, ok := finishToStop[choice.FinishReason]; ok {
			stop = s
		}
		if len(choice.Message.ToolCalls) > 0 {
			stop = typeToolUse
		}
	}
	out := map[string]any{
		"id": in.ID, "type": "message", "role": roleAssistant, "model": in.Model,
		"content": content, "stop_reason": stop, stopSequence: nil,
		keyUsage: map[string]any{"input_tokens": in.Usage.PromptTokens, "output_tokens": in.Usage.CompletionTokens},
	}
	return json.Marshal(out)
}

// Error rewrites an error body into the caller's envelope shape. An
// unparseable body becomes a generic envelope carrying it as the message.
func Error(from, to Format, body []byte) []byte {
	if from == to {
		return body
	}
	var errType, message string
	switch from {
	case FormatAnthropic:
		var in struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &in) == nil && in.Error.Message != "" {
			errType, message = in.Error.Type, in.Error.Message
		}
	case FormatOpenAI:
		var in struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &in) == nil && in.Error.Message != "" {
			errType, message = in.Error.Type, in.Error.Message
		}
	}
	if message == "" {
		message = string(body)
	}
	if errType == "" {
		errType = "invalid_request_error"
	}
	var out []byte
	if to == FormatAnthropic {
		out, _ = json.Marshal(map[string]any{
			"type": "error", "error": map[string]any{"type": errType, "message": message}})
	} else {
		out, _ = json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": errType, "code": nil}})
	}
	return out
}
