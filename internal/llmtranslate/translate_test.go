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
	"errors"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad JSON in test: %v\n%s", err, s)
	}
	return m
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// path digs into nested maps and slices with a dotted path ("messages.0.role").
func path(v any, p string) any {
	for _, seg := range strings.Split(p, ".") {
		switch cur := v.(type) {
		case map[string]any:
			v = cur[seg]
		case []any:
			idx := 0
			for _, ch := range seg {
				idx = idx*10 + int(ch-'0')
			}
			if idx >= len(cur) {
				return nil
			}
			v = cur[idx]
		default:
			return nil
		}
	}
	return v
}

// ---- requests: Anthropic -> OpenAI, one case per matrix row ----

func TestRequest_AnthropicToOpenAI(t *testing.T) {
	in := parse(t, `{
	  "model": "claude-sonnet-4-6", "max_tokens": 1024, "temperature": 0.2, "top_p": 0.9, "top_k": 40,
	  "stop_sequences": ["END"], "stream": true, "metadata": {"user_id": "u-1"},
	  "system": [{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Be kind."}],
	  "tools": [{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
	  "tool_choice": {"type":"tool","name":"get_weather","disable_parallel_tool_use":true},
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
	    {"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Oslo"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"12C"}],"is_error":true},{"type":"text","text":"Thanks"}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Cold."}]}
	  ]}`)
	out, err := Request(FormatAnthropic, FormatOpenAI, in, "gpt-5-mini", 0)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"model": "gpt-5-mini", "max_tokens": float64(1024), "temperature": 0.2, "top_p": 0.9,
		"stream": true, "user": "u-1", "tool_choice.type": "function", "tool_choice.function.name": "get_weather",
		"parallel_tool_calls": false,
		"messages.0.role":     "system", "messages.0.content": "Be brief.\n\nBe kind.",
		"messages.1.role": "user", "messages.1.content.0.text": "Hi",
		"messages.1.content.1.image_url.url": "data:image/png;base64,AAAA",
		"messages.2.role":                    "assistant", "messages.2.content": "Checking.",
		"messages.2.tool_calls.0.id": "tu_1", "messages.2.tool_calls.0.function.arguments": `{"city":"Oslo"}`,
		"messages.3.role": "tool", "messages.3.tool_call_id": "tu_1", "messages.3.content": "[error] 12C",
		"messages.4.role": "user", "messages.4.content": "Thanks",
		"messages.5.role": "assistant", "messages.5.content": "Cold.",
		"tools.0.type": "function", "tools.0.function.name": "get_weather",
		"tools.0.function.parameters.properties.city.type": "string",
		"stop.0": "END",
	}
	for p, want := range checks {
		if got := path(out, p); got != want {
			t.Errorf("%s = %#v, want %#v", p, got, want)
		}
	}
	if _, present := out["top_k"]; present {
		t.Error("top_k must drop")
	}
	if strings.Contains(mustJSON(t, out), "cache_control") {
		t.Error("cache_control must drop")
	}
	// Thinking blocks in history drop rather than block the crossing.
	if strings.Contains(mustJSON(t, out), "hmm") {
		t.Error("prior-turn thinking must drop")
	}
}

func TestRequest_AnthropicToOpenAI_ToolChoiceForms(t *testing.T) {
	for in, want := range map[string]any{
		`{"type":"auto"}`: "auto", `{"type":"any"}`: "required", `{"type":"none"}`: "none",
	} {
		body := parse(t, `{"model":"m","max_tokens":10,"messages":[],"tool_choice":`+in+`}`)
		out, err := Request(FormatAnthropic, FormatOpenAI, body, "g", 0)
		if err != nil || out["tool_choice"] != want {
			t.Errorf("%s -> %v (err %v), want %v", in, out["tool_choice"], err, want)
		}
	}
}

func TestRequest_AnthropicToOpenAI_Untranslatable(t *testing.T) {
	cases := map[string]string{
		`{"model":"m","max_tokens":1,"messages":[],"thinking":{"type":"enabled","budget_tokens":1024}}`:           "extended thinking",
		`{"model":"m","max_tokens":1,"messages":[],"mcp_servers":[{"name":"x"}]}`:                                 "MCP servers",
		`{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`: "server tool",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`:   "document",
		`{"model":"m","max_tokens":1,"messages":[],"output_format":{"type":"json_schema"}}`:                       "structured outputs",
	}
	for body, feature := range cases {
		_, err := Request(FormatAnthropic, FormatOpenAI, parse(t, body), "g", 0)
		var u *Untranslatable
		if !errors.As(err, &u) || !strings.Contains(u.Feature, feature) {
			t.Errorf("%s: err = %v, want Untranslatable naming %q", body, err, feature)
		}
	}
}

// ---- requests: OpenAI -> Anthropic ----

func TestRequest_OpenAIToAnthropic(t *testing.T) {
	in := parse(t, `{
	  "model": "gpt-5-mini", "max_completion_tokens": 20000, "temperature": 0.5, "top_p": 0.8, "stop": "END",
	  "stream": true, "user": "u-2", "seed": 7, "presence_penalty": 0.1, "stream_options": {"include_usage": true},
	  "tools": [{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object"},"strict":true}}],
	  "tool_choice": {"type":"function","function":{"name":"get_weather"}}, "parallel_tool_calls": false,
	  "messages": [
	    {"role":"developer","content":"Be brief."},
	    {"role":"system","content":[{"type":"text","text":"Be kind."}]},
	    {"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]},
	    {"role":"assistant","content":"Checking.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"12C"},
	    {"role":"user","content":"Thanks"}
	  ]}`)
	out, err := Request(FormatOpenAI, FormatAnthropic, in, "claude-sonnet-4-6", 8192)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": int64(8192), "temperature": 0.5, "top_p": 0.8,
		"stream": true, "metadata.user_id": "u-2", "system": "Be brief.\n\nBe kind.",
		"stop_sequences.0": "END",
		"tools.0.name":     "get_weather", "tools.0.input_schema.type": "object",
		"tool_choice.type": "tool", "tool_choice.name": "get_weather", "tool_choice.disable_parallel_tool_use": true,
		"messages.0.role": "user", "messages.0.content.0.text": "Hi",
		"messages.0.content.1.source.type": "base64", "messages.0.content.1.source.media_type": "image/png",
		"messages.0.content.1.source.data": "AAAA", "messages.0.content.2.source.type": "url",
		"messages.1.role": "assistant", "messages.1.content.0.text": "Checking.",
		"messages.1.content.1.type": "tool_use", "messages.1.content.1.id": "call_1", "messages.1.content.1.input.city": "Oslo",
		// The tool result and the following user text merge into ONE user message.
		"messages.2.role": "user", "messages.2.content.0.type": "tool_result", "messages.2.content.0.tool_use_id": "call_1",
		"messages.2.content.0.content": "12C", "messages.2.content.1.text": "Thanks",
	}
	for p, want := range checks {
		if got := path(out, p); got != want {
			t.Errorf("%s = %#v, want %#v", p, got, want)
		}
	}
	if msgs, _ := out["messages"].([]any); len(msgs) != 3 {
		t.Errorf("messages = %d, want 3 (system lifted, tool result merged)", len(msgs))
	}
	for _, dropped := range []string{"seed", "presence_penalty", "stream_options", "strict"} {
		if strings.Contains(mustJSON(t, out), `"`+dropped+`"`) {
			t.Errorf("%s must drop", dropped)
		}
	}
}

func TestRequest_OpenAIToAnthropic_MaxTokens(t *testing.T) {
	base := `{"model":"g","messages":[{"role":"user","content":"hi"}]`
	// Omitted, ceiling known: supplied.
	out, err := Request(FormatOpenAI, FormatAnthropic, parse(t, base+`}`), "c", 4096)
	if err != nil || out["max_tokens"] != int64(4096) {
		t.Errorf("omitted with ceiling: %v %v", out["max_tokens"], err)
	}
	// Omitted, ceiling unknown: untranslatable, naming the fix.
	_, err = Request(FormatOpenAI, FormatAnthropic, parse(t, base+`}`), "c", 0)
	var u *Untranslatable
	if !errors.As(err, &u) || !strings.Contains(u.Feature, "maxOutputTokens") {
		t.Errorf("omitted without ceiling: %v", err)
	}
	// Present and below the ceiling: passes through.
	out, _ = Request(FormatOpenAI, FormatAnthropic, parse(t, base+`,"max_tokens":100}`), "c", 4096)
	if out["max_tokens"] != int64(100) {
		t.Errorf("present below ceiling: %v", out["max_tokens"])
	}
	// Present and above the ceiling: capped.
	out, _ = Request(FormatOpenAI, FormatAnthropic, parse(t, base+`,"max_tokens":9999}`), "c", 4096)
	if out["max_tokens"] != int64(4096) {
		t.Errorf("present above ceiling: %v", out["max_tokens"])
	}
	// Present, no ceiling: passes through.
	out, _ = Request(FormatOpenAI, FormatAnthropic, parse(t, base+`,"max_tokens":300}`), "c", 0)
	if out["max_tokens"] != int64(300) {
		t.Errorf("present without ceiling: %v", out["max_tokens"])
	}
}

func TestRequest_OpenAIToAnthropic_ToolChoiceForms(t *testing.T) {
	for in, want := range map[string]string{`"auto"`: "auto", `"required"`: "any", `"none"`: "none"} {
		body := parse(t, `{"model":"m","max_tokens":10,"messages":[],"tool_choice":`+in+`}`)
		out, err := Request(FormatOpenAI, FormatAnthropic, body, "c", 0)
		if err != nil || path(out, "tool_choice.type") != want {
			t.Errorf("%s -> %v (err %v), want %v", in, out["tool_choice"], err, want)
		}
	}
}

func TestRequest_OpenAIToAnthropic_Untranslatable(t *testing.T) {
	cases := map[string]string{
		`{"model":"m","max_tokens":1,"messages":[],"n":2}`:                                                              "n greater than 1",
		`{"model":"m","max_tokens":1,"messages":[],"logprobs":true}`:                                                    "logprobs",
		`{"model":"m","max_tokens":1,"messages":[],"response_format":{"type":"json_object"}}`:                           "response_format",
		`{"model":"m","max_tokens":1,"messages":[],"functions":[{"name":"f"}]}`:                                         "functions",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`: "audio",
		`{"model":"m","max_tokens":1,"messages":[],"tools":[{"type":"file_search"}]}`:                                   "tool type",
	}
	for body, feature := range cases {
		_, err := Request(FormatOpenAI, FormatAnthropic, parse(t, body), "c", 0)
		var u *Untranslatable
		if !errors.As(err, &u) || !strings.Contains(u.Feature, feature) {
			t.Errorf("%s: err = %v, want Untranslatable naming %q", body, err, feature)
		}
	}
	// n: 1 and logprobs: false are fine.
	if _, err := Request(FormatOpenAI, FormatAnthropic,
		parse(t, `{"model":"m","max_tokens":1,"messages":[],"n":1,"logprobs":false}`), "c", 0); err != nil {
		t.Errorf("n:1 / logprobs:false must translate: %v", err)
	}
}

func TestRequest_SameFormatOnlyRenamesTheModel(t *testing.T) {
	in := parse(t, `{"model":"a","messages":[],"stream":true}`)
	out, err := Request(FormatOpenAI, FormatOpenAI, in, "b", 0)
	if err != nil || out["model"] != "b" || out["stream"] != true || in["model"] != "a" {
		t.Errorf("same-format: %v %v (input mutated: %v)", out, err, in["model"])
	}
}

// ---- responses ----

func TestResponse_AnthropicToOpenAI(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",
	  "content":[{"type":"text","text":"It is "},{"type":"text","text":"cold."},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Oslo"}}],
	  "stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3}}`)
	out, err := Response(FormatAnthropic, FormatOpenAI, body)
	if err != nil {
		t.Fatal(err)
	}
	got := parse(t, string(out))
	checks := map[string]any{
		"id": "msg_1", "object": "chat.completion", "model": "claude-sonnet-4-6",
		"choices.0.message.role": "assistant", "choices.0.message.content": "It is \ncold.",
		"choices.0.message.tool_calls.0.id": "tu_1", "choices.0.message.tool_calls.0.function.arguments": `{"city":"Oslo"}`,
		"choices.0.finish_reason": "tool_calls",
		"usage.prompt_tokens":     float64(13), "usage.completion_tokens": float64(5), "usage.total_tokens": float64(18),
	}
	for p, want := range checks {
		if v := path(got, p); v != want {
			t.Errorf("%s = %#v, want %#v", p, v, want)
		}
	}
	for stop, finish := range map[string]string{"end_turn": "stop", "max_tokens": "length", "refusal": "content_filter", "stop_sequence": "stop"} {
		out, _ := Response(FormatAnthropic, FormatOpenAI, []byte(`{"content":[],"stop_reason":"`+stop+`","usage":{}}`))
		if f := path(parse(t, string(out)), "choices.0.finish_reason"); f != finish {
			t.Errorf("%s -> %v, want %s", stop, f, finish)
		}
	}
}

func TestResponse_OpenAIToAnthropic(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5-mini",
	  "choices":[{"index":0,"message":{"role":"assistant","content":"Cold.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],
	  "usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	out, err := Response(FormatOpenAI, FormatAnthropic, body)
	if err != nil {
		t.Fatal(err)
	}
	got := parse(t, string(out))
	checks := map[string]any{
		"id": "chatcmpl-1", "type": "message", "role": "assistant", "model": "gpt-5-mini",
		"content.0.type": "text", "content.0.text": "Cold.",
		"content.1.type": "tool_use", "content.1.id": "call_1", "content.1.name": "get_weather", "content.1.input.city": "Oslo",
		"stop_reason": "tool_use", "usage.input_tokens": float64(10), "usage.output_tokens": float64(5),
	}
	for p, want := range checks {
		if v := path(got, p); v != want {
			t.Errorf("%s = %#v, want %#v", p, v, want)
		}
	}
	for finish, stop := range map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal"} {
		out, _ := Response(FormatOpenAI, FormatAnthropic, []byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"`+finish+`"}],"usage":{}}`))
		if s := path(parse(t, string(out)), "stop_reason"); s != stop {
			t.Errorf("%s -> %v, want %s", finish, s, stop)
		}
	}
}

func TestError_Envelopes(t *testing.T) {
	out := Error(FormatOpenAI, FormatAnthropic, []byte(`{"error":{"message":"bad prompt","type":"invalid_request_error","code":null}}`))
	if got := parse(t, string(out)); got["type"] != "error" || path(got, "error.message") != "bad prompt" {
		t.Errorf("openai->anthropic: %s", out)
	}
	out = Error(FormatAnthropic, FormatOpenAI, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"too long"}}`))
	if got := parse(t, string(out)); path(got, "error.message") != "too long" || path(got, "error.type") != "invalid_request_error" {
		t.Errorf("anthropic->openai: %s", out)
	}
	out = Error(FormatAnthropic, FormatOpenAI, []byte(`not json`))
	if got := parse(t, string(out)); path(got, "error.message") != "not json" {
		t.Errorf("unparseable: %s", out)
	}
}

// ---- streams ----

func feedAll(s Stream, lines ...string) []string {
	var out []string
	for _, l := range lines {
		for _, o := range s.Feed([]byte(l)) {
			out = append(out, string(o))
		}
	}
	return out
}

func dataPayloads(t *testing.T, lines []string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "data: "); ok && rest != "[DONE]" {
			out = append(out, parse(t, rest))
		}
	}
	return out
}

func TestStream_OpenAIToAnthropic(t *testing.T) {
	s := NewStream(FormatOpenAI, FormatAnthropic, "gpt-5-mini")
	lines := feedAll(s,
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-5-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"It is"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":" cold."},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":7}}`,
		`data: [DONE]`,
	)
	var events []string
	for _, l := range lines {
		if name, ok := strings.CutPrefix(l, "event: "); ok {
			events = append(events, name)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant %v", events, want)
	}
	payloads := dataPayloads(t, lines)
	if path(payloads[0], "message.model") != "gpt-5-mini" || path(payloads[0], "message.usage.input_tokens") != float64(0) {
		t.Errorf("message_start = %v", payloads[0])
	}
	if path(payloads[2], "delta.text") != "It is" || path(payloads[5], "content_block.name") != "get_weather" ||
		path(payloads[5], "content_block.id") != "call_1" || path(payloads[6], "delta.partial_json") != `{"city":` {
		t.Errorf("block payloads wrong: %v %v %v", payloads[2], payloads[5], payloads[6])
	}
	delta := payloads[9]
	if path(delta, "delta.stop_reason") != "tool_use" || path(delta, "usage.output_tokens") != float64(7) ||
		path(delta, "usage.input_tokens") != float64(10) {
		t.Errorf("message_delta = %v", delta)
	}
	// Feeding after the end is silent.
	if extra := s.Feed([]byte(`data: {}`)); len(extra) != 0 {
		t.Errorf("post-DONE feed produced %v", extra)
	}
}

func TestStream_OpenAIToAnthropic_EndsWithoutUsageChunk(t *testing.T) {
	s := NewStream(FormatOpenAI, FormatAnthropic, "m")
	lines := feedAll(s,
		`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	for _, l := range s.Finish() {
		lines = append(lines, string(l))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "event: message_delta") || !strings.HasSuffix(strings.TrimSpace(joined), `{"type":"message_stop"}`) {
		t.Errorf("stream without usage chunk must still close:\n%s", joined)
	}
	if strings.Contains(joined, `"stop_reason":"tool_use"`) || !strings.Contains(joined, `"stop_reason":"end_turn"`) {
		t.Errorf("stop reason wrong:\n%s", joined)
	}
}

func TestStream_AnthropicToOpenAI(t *testing.T) {
	s := NewStream(FormatAnthropic, FormatOpenAI, "claude-sonnet-4-6")
	lines := feedAll(s,
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Cold."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Oslo\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	payloads := dataPayloads(t, lines)
	if len(payloads) != 6 {
		t.Fatalf("chunks = %d: %v", len(payloads), lines)
	}
	checks := []struct {
		i    int
		p    string
		want any
	}{
		{0, "choices.0.delta.role", "assistant"}, {0, "model", "claude-sonnet-4-6"}, {0, "object", "chat.completion.chunk"},
		{1, "choices.0.delta.content", "Cold."},
		{2, "choices.0.delta.tool_calls.0.id", "tu_1"}, {2, "choices.0.delta.tool_calls.0.function.name", "get_weather"},
		{2, "choices.0.delta.tool_calls.0.index", float64(0)},
		{3, "choices.0.delta.tool_calls.0.function.arguments", `{"city":"Oslo"}`},
		{4, "choices.0.finish_reason", "tool_calls"},
		{5, "usage.prompt_tokens", float64(10)}, {5, "usage.completion_tokens", float64(9)}, {5, "usage.total_tokens", float64(19)},
	}
	for _, c := range checks {
		if v := path(payloads[c.i], c.p); v != c.want {
			t.Errorf("chunk %d %s = %#v, want %#v", c.i, c.p, v, c.want)
		}
	}
	if lines[len(lines)-2] != "data: [DONE]" {
		t.Errorf("stream must end with [DONE]: %v", lines[len(lines)-3:])
	}
	if extra := s.Feed([]byte(`event: message_stop`)); len(extra) != 0 {
		t.Errorf("post-stop feed produced %v", extra)
	}
}

func TestStream_AnthropicToOpenAI_ErrorEvent(t *testing.T) {
	s := NewStream(FormatAnthropic, FormatOpenAI, "m")
	lines := feedAll(s,
		`event: message_start`, `data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`, ``,
		`event: error`, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ``,
	)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"error":{"message":"Overloaded"`) || !strings.HasSuffix(strings.TrimSpace(joined), "data: [DONE]") {
		t.Errorf("error event must become an error chunk then DONE:\n%s", joined)
	}
}

func TestNewStream_SameFormatIsNil(t *testing.T) {
	if NewStream(FormatOpenAI, FormatOpenAI, "m") != nil || NewStream(FormatAnthropic, FormatAnthropic, "m") != nil {
		t.Error("same-format streams must not be translated")
	}
}
