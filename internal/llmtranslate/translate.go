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

// Package llmtranslate rewrites LLM requests and responses between the
// Anthropic messages format and the OpenAI chat completions format, for a
// fallback edge that crosses them (docs/src/gateways/llm/fallback.md,
// Crossing formats). It has no gateway dependencies: the gateway hands it
// parsed bodies, SSE lines, and the target model, and gets bytes back.
package llmtranslate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format is a wire format the gateway speaks.
type Format string

const (
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
)

// Untranslatable names a request feature the target format cannot express.
// The gateway treats it as ineligibility for this request: no attempt slot,
// a FallbackIneligible event naming Feature.
type Untranslatable struct{ Feature string }

func (u *Untranslatable) Error() string { return "request uses " + u.Feature }

func untranslatable(feature string) error { return &Untranslatable{Feature: feature} }

// Repeated wire keys and values, named once for the linter and the reader.
const (
	roleAssistant = "assistant"
	typeText      = "text"
	typeFunction  = "function"
	typeToolUse   = "tool_use"
	keyArguments  = "arguments"
	keyCreated    = "created"
	keyChoices    = "choices"
	keyUsage      = "usage"
	keyInput      = "input"
	keyToolCalls  = "tool_calls"
	stopEndTurn   = "end_turn"
	stopSequence  = "stop_sequence"
)

// Request rewrites a parsed request body from one format to the other for
// model. maxOutputTokens is the target model's declared ceiling (0 when it
// declares none): Anthropic requires max_tokens, so an OpenAI request
// without one is untranslatable when the ceiling is unknown, and a value
// above a known ceiling is capped to it.
func Request(from, to Format, body map[string]any, model string, maxOutputTokens int64) (map[string]any, error) {
	switch {
	case from == to:
		out := cloneMap(body)
		out["model"] = model
		return out, nil
	case from == FormatAnthropic && to == FormatOpenAI:
		return anthropicToOpenAIRequest(body, model)
	case from == FormatOpenAI && to == FormatAnthropic:
		return openAIToAnthropicRequest(body, model, maxOutputTokens)
	}
	return nil, fmt.Errorf("no translation from %s to %s", from, to)
}

// ---- Anthropic messages -> OpenAI chat completions ----

func anthropicToOpenAIRequest(in map[string]any, model string) (map[string]any, error) {
	for _, feature := range []struct{ key, name string }{
		{"thinking", "extended thinking"}, {"mcp_servers", "MCP servers"},
		{"container", "a container"}, {"output_format", "structured outputs"},
	} {
		if _, ok := in[feature.key]; ok {
			return nil, untranslatable(feature.name + ", which openai cannot express")
		}
	}
	out := map[string]any{"model": model}
	var messages []any

	if system, ok := in["system"]; ok {
		text, err := anthropicSystemText(system)
		if err != nil {
			return nil, err
		}
		if text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	inMessages, _ := in["messages"].([]any)
	for _, raw := range inMessages {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		converted, err := anthropicMessageToOpenAI(role, m["content"])
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages

	if tools, ok := in["tools"].([]any); ok {
		var outTools []any
		for _, raw := range tools {
			t, _ := raw.(map[string]any)
			if typ, ok := t["type"].(string); ok && typ != "custom" && typ != "" {
				return nil, untranslatable(fmt.Sprintf("the server tool type %q, which openai cannot express", typ))
			}
			fn := map[string]any{"name": t["name"]}
			if d, ok := t["description"]; ok {
				fn["description"] = d
			}
			if schema, ok := t["input_schema"]; ok {
				fn["parameters"] = schema
			}
			outTools = append(outTools, map[string]any{"type": typeFunction, typeFunction: fn})
		}
		out["tools"] = outTools
	}
	if tc, ok := in["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = map[string]any{"type": typeFunction, typeFunction: map[string]any{"name": tc["name"]}}
		}
		if disable, ok := tc["disable_parallel_tool_use"].(bool); ok && disable {
			out["parallel_tool_calls"] = false
		}
	}

	copyIf(in, out, "max_tokens", "max_tokens")
	copyIf(in, out, "temperature", "temperature")
	copyIf(in, out, "top_p", "top_p")
	copyIf(in, out, "stop_sequences", "stop")
	copyIf(in, out, "stream", "stream")
	if md, ok := in["metadata"].(map[string]any); ok {
		if user, ok := md["user_id"]; ok {
			out["user"] = user
		}
	}
	// top_k has no OpenAI counterpart and cache_control is a hint: both drop.
	return out, nil
}

func anthropicSystemText(system any) (string, error) {
	switch v := system.(type) {
	case string:
		return v, nil
	case []any:
		var parts []string
		for _, raw := range v {
			b, _ := raw.(map[string]any)
			if b["type"] != typeText {
				return "", untranslatable("a non-text system block, which openai cannot express")
			}
			if t, ok := b[typeText].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n"), nil
	}
	return "", nil
}

// anthropicMessageToOpenAI converts one message. A user message carrying
// tool_result blocks becomes tool messages (one per result) followed by a
// user message for any remaining content, because OpenAI's tool results are
// messages of their own.
func anthropicMessageToOpenAI(role string, content any) ([]any, error) {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	blocks, _ := content.([]any)
	var toolMessages []any
	var parts []any
	var toolCalls []any
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		switch b["type"] {
		case typeText:
			parts = append(parts, map[string]any{"type": typeText, typeText: b[typeText]})
		case "image":
			url, err := anthropicImageURL(b)
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case typeToolUse:
			args, _ := json.Marshal(b[keyInput])
			toolCalls = append(toolCalls, map[string]any{
				"id": b["id"], "type": typeFunction,
				typeFunction: map[string]any{"name": b["name"], keyArguments: string(args)},
			})
		case "tool_result":
			text := anthropicToolResultText(b)
			if isErr, _ := b["is_error"].(bool); isErr {
				text = "[error] " + text
			}
			toolMessages = append(toolMessages, map[string]any{
				"role": "tool", "tool_call_id": b["tool_use_id"], "content": text,
			})
		case "thinking", "redacted_thinking":
			// Prior-turn reasoning; dropped, as Anthropic itself does when
			// thinking is off. The request-level thinking flag is what is
			// untranslatable.
		case "document":
			return nil, untranslatable("a document block, which openai cannot express")
		default:
			return nil, untranslatable(fmt.Sprintf("a %v content block, which openai cannot express", b["type"]))
		}
	}
	out := toolMessages
	if role == roleAssistant {
		msg := map[string]any{"role": roleAssistant}
		if text := joinTextParts(parts); text != "" {
			msg["content"] = text
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg[keyToolCalls] = toolCalls
		}
		return append(out, msg), nil
	}
	if len(parts) > 0 {
		if allText(parts) {
			out = append(out, map[string]any{"role": role, "content": joinTextParts(parts)})
		} else {
			out = append(out, map[string]any{"role": role, "content": parts})
		}
	}
	return out, nil
}

func anthropicImageURL(b map[string]any) (string, error) {
	src, _ := b["source"].(map[string]any)
	switch src["type"] {
	case "base64":
		return fmt.Sprintf("data:%v;base64,%v", src["media_type"], src["data"]), nil
	case "url":
		u, _ := src["url"].(string)
		return u, nil
	}
	return "", untranslatable("an image source openai cannot express")
}

func anthropicToolResultText(b map[string]any) string {
	switch c := b["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			blk, _ := raw.(map[string]any)
			if t, ok := blk[typeText].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ---- OpenAI chat completions -> Anthropic messages ----

func openAIToAnthropicRequest(in map[string]any, model string, maxOutputTokens int64) (map[string]any, error) {
	if err := openAIUntranslatable(in); err != nil {
		return nil, err
	}
	out := map[string]any{"model": model}
	system, messages, err := openAIMessagesToAnthropic(in["messages"])
	if err != nil {
		return nil, err
	}
	if system != "" {
		out["system"] = system
	}
	out["messages"] = messages

	if tools, ok := in["tools"].([]any); ok {
		outTools, err := openAIToolsToAnthropic(tools)
		if err != nil {
			return nil, err
		}
		out["tools"] = outTools
	}
	if toolChoice := openAIToolChoiceToAnthropic(in); toolChoice != nil {
		out["tool_choice"] = toolChoice
	}
	maxTokens, err := openAIMaxTokens(in, model, maxOutputTokens)
	if err != nil {
		return nil, err
	}
	out["max_tokens"] = maxTokens
	copyIf(in, out, "temperature", "temperature")
	copyIf(in, out, "top_p", "top_p")
	copyIf(in, out, "stream", "stream")
	switch stop := in["stop"].(type) {
	case string:
		out["stop_sequences"] = []any{stop}
	case []any:
		out["stop_sequences"] = stop
	}
	if user, ok := in["user"]; ok {
		out["metadata"] = map[string]any{"user_id": user}
	}
	// seed, presence_penalty, frequency_penalty, logit_bias, and
	// stream_options have no Anthropic counterpart and drop.
	return out, nil
}

// openAIUntranslatable rejects the request-level features Anthropic cannot
// express.
func openAIUntranslatable(in map[string]any) error {
	if n, ok := numberOf(in["n"]); ok && n > 1 {
		return untranslatable("n greater than 1, which anthropic cannot express")
	}
	for _, feature := range []struct{ key, name string }{
		{"logprobs", "logprobs"}, {"top_logprobs", "logprobs"}, {"response_format", "response_format"},
		{"functions", "the legacy functions field"}, {"function_call", "the legacy function_call field"},
	} {
		if v, ok := in[feature.key]; ok && v != nil && v != false {
			return untranslatable(feature.name + ", which anthropic cannot express")
		}
	}
	return nil
}

// openAIMessagesToAnthropic lifts system and developer messages into the
// system prompt and converts the rest. Tool results become tool_result
// blocks on a user message; consecutive user content merges into one
// message, as Anthropic requires all of a turn's results together.
func openAIMessagesToAnthropic(raw any) (string, []any, error) {
	var system []string
	var messages []map[string]any
	appendUser := func(blocks []any) {
		if n := len(messages); n > 0 && messages[n-1]["role"] == "user" {
			existing, _ := messages[n-1]["content"].([]any)
			messages[n-1]["content"] = append(existing, blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": "user", "content": blocks})
	}
	inMessages, _ := raw.([]any)
	for _, rawMsg := range inMessages {
		m, _ := rawMsg.(map[string]any)
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			system = append(system, contentText(m["content"]))
		case "user":
			blocks, err := openAIContentToBlocks(m["content"])
			if err != nil {
				return "", nil, err
			}
			appendUser(blocks)
		case roleAssistant:
			blocks, err := openAIAssistantBlocks(m)
			if err != nil {
				return "", nil, err
			}
			messages = append(messages, map[string]any{"role": roleAssistant, "content": blocks})
		case "tool":
			appendUser([]any{map[string]any{
				"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": contentText(m["content"]),
			}})
		default:
			return "", nil, untranslatable(fmt.Sprintf("the %q message role, which anthropic cannot express", role))
		}
	}
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, m)
	}
	return strings.Join(system, "\n\n"), out, nil
}

func openAIAssistantBlocks(m map[string]any) ([]any, error) {
	var blocks []any
	if text := contentText(m["content"]); text != "" {
		blocks = append(blocks, map[string]any{"type": typeText, typeText: text})
	}
	calls, _ := m[keyToolCalls].([]any)
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		fn, _ := call[typeFunction].(map[string]any)
		var input any = map[string]any{}
		if args, ok := fn[keyArguments].(string); ok && strings.TrimSpace(args) != "" {
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				return nil, untranslatable("tool call arguments that are not JSON")
			}
		}
		blocks = append(blocks, map[string]any{
			"type": typeToolUse, "id": call["id"], "name": fn["name"], keyInput: input,
		})
	}
	return blocks, nil
}

func openAIToolsToAnthropic(tools []any) ([]any, error) {
	var outTools []any
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if t["type"] != typeFunction {
			return nil, untranslatable(fmt.Sprintf("the tool type %v, which anthropic cannot express", t["type"]))
		}
		fn, _ := t[typeFunction].(map[string]any)
		tool := map[string]any{"name": fn["name"]}
		if d, ok := fn["description"]; ok {
			tool["description"] = d
		}
		if params, ok := fn["parameters"]; ok {
			tool["input_schema"] = params
		} else {
			tool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		outTools = append(outTools, tool)
	}
	return outTools, nil
}

func openAIToolChoiceToAnthropic(in map[string]any) map[string]any {
	var toolChoice map[string]any
	switch tc := in["tool_choice"].(type) {
	case string:
		switch tc {
		case "auto":
			toolChoice = map[string]any{"type": "auto"}
		case "required":
			toolChoice = map[string]any{"type": "any"}
		case "none":
			toolChoice = map[string]any{"type": "none"}
		}
	case map[string]any:
		fn, _ := tc[typeFunction].(map[string]any)
		toolChoice = map[string]any{"type": "tool", "name": fn["name"]}
	}
	if parallel, ok := in["parallel_tool_calls"].(bool); ok && !parallel {
		if toolChoice == nil {
			toolChoice = map[string]any{"type": "auto"}
		}
		toolChoice["disable_parallel_tool_use"] = true
	}
	return toolChoice
}

// openAIMaxTokens applies the max_tokens rule: the request's value capped
// to a known ceiling, the ceiling when the request has none, and
// untranslatable when neither exists (Anthropic requires the field).
func openAIMaxTokens(in map[string]any, model string, maxOutputTokens int64) (int64, error) {
	maxTokens, hasMax := numberOf(in["max_completion_tokens"])
	if !hasMax {
		maxTokens, hasMax = numberOf(in["max_tokens"])
	}
	switch {
	case !hasMax && maxOutputTokens <= 0:
		return 0, untranslatable(fmt.Sprintf(
			"no max_tokens, which anthropic requires, and model %q declares no maxOutputTokens", model))
	case !hasMax:
		return maxOutputTokens, nil
	case maxOutputTokens > 0 && maxTokens > maxOutputTokens:
		return maxOutputTokens, nil
	}
	return maxTokens, nil
}

func openAIContentToBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case string:
		return []any{map[string]any{"type": typeText, typeText: c}}, nil
	case []any:
		var blocks []any
		for _, raw := range c {
			part, _ := raw.(map[string]any)
			switch part["type"] {
			case typeText:
				blocks = append(blocks, map[string]any{"type": typeText, typeText: part[typeText]})
			case "image_url":
				iu, _ := part["image_url"].(map[string]any)
				u, _ := iu["url"].(string)
				blocks = append(blocks, map[string]any{"type": "image", "source": imageSource(u)})
			case "input_audio":
				return nil, untranslatable("audio content, which anthropic cannot express")
			default:
				return nil, untranslatable(fmt.Sprintf("a %v content part, which anthropic cannot express", part["type"]))
			}
		}
		return blocks, nil
	}
	return nil, nil
}

func imageSource(u string) map[string]any {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			mediaType := strings.TrimSuffix(meta, ";base64")
			return map[string]any{"type": "base64", "media_type": mediaType, "data": data}
		}
	}
	return map[string]any{"type": "url", "url": u}
}

// ---- helpers ----

func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			part, _ := raw.(map[string]any)
			if t, ok := part[typeText].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func joinTextParts(parts []any) string {
	var texts []string
	for _, raw := range parts {
		p, _ := raw.(map[string]any)
		if t, ok := p[typeText].(string); ok {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

func allText(parts []any) bool {
	for _, raw := range parts {
		p, _ := raw.(map[string]any)
		if p["type"] != typeText {
			return false
		}
	}
	return true
}

func copyIf(in, out map[string]any, from, to string) {
	if v, ok := in[from]; ok && v != nil {
		out[to] = v
	}
}

func numberOf(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
