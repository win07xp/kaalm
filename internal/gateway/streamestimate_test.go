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
	"strings"
	"testing"
)

// estimateOf feeds events to the adapter's estimator for request and returns
// its estimate.
func estimateOf(t *testing.T, providerType, request string, events ...string) (input, output int64) {
	t.Helper()
	a, ok := adapterForProviderType(providerType)
	if !ok {
		t.Fatalf("no adapter for %q", providerType)
	}
	est := a.streamEstimator([]byte(request))
	if est == nil {
		t.Fatalf("the %s adapter estimates nothing", providerType)
	}
	for _, e := range events {
		est.event([]byte(e))
	}
	return est.estimate()
}

// The input estimate is the body the provider received over four, for every
// adapter.
func TestStreamEstimator_InputIsRequestBytesOverFour(t *testing.T) {
	request := strings.Repeat("x", 41)
	for _, typ := range []string{providerTypeAnthropic, providerTypeOpenAI, providerTypeOpenAICompatible, providerTypeVertex} {
		if in, out := estimateOf(t, typ, request); in != 10 || out != 0 {
			t.Errorf("%s: estimate = %d, %d; want 10, 0", typ, in, out)
		}
	}
}

// Each event adds its generated content over four, at least one token when
// it holds any; events with no content add nothing.
func TestStreamEstimator_OutputPerEvent(t *testing.T) {
	cases := []struct {
		name, typ, event string
		want             int64
	}{
		{"openai short content", providerTypeOpenAI, `{"choices":[{"delta":{"content":"hel"}}]}`, 1},
		{"openai 20-byte content", providerTypeOpenAI, `{"choices":[{"delta":{"content":"aaaaaaaaaaaaaaaaaaaa"}}]}`, 5},
		{"openai tool arguments", providerTypeOpenAI,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}`, 5},
		{"openai refusal", providerTypeOpenAI, `{"choices":[{"delta":{"refusal":"no thanks"}}]}`, 2},
		{"openai reasoning_content", providerTypeOpenAI, `{"choices":[{"delta":{"reasoning_content":"think hard"}}]}`, 2},
		{"openai reasoning", providerTypeOpenAI, `{"choices":[{"delta":{"reasoning":"think"}}]}`, 1},
		{"openai role chunk", providerTypeOpenAI, `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, 0},
		{"openai null content", providerTypeOpenAI, `{"choices":[{"delta":{"content":null}}]}`, 0},
		{"openai usage chunk", providerTypeOpenAI,
			`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":200,"total_tokens":212}}`, 0},
		{"openai finish chunk", providerTypeOpenAI, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, 0},
		{"openai done", providerTypeOpenAI, `[DONE]`, 0},
		{"anthropic text delta", providerTypeAnthropic,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello world!"}}`, 3},
		{"anthropic input json delta", providerTypeAnthropic,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"k\":\"a\\\"b\"}"}}`, 4},
		{"anthropic thinking delta", providerTypeAnthropic,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me see"}}`, 2},
		{"anthropic message_start", providerTypeAnthropic,
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}`, 0},
		{"anthropic empty text block start", providerTypeAnthropic,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, 0},
		{"anthropic message_delta", providerTypeAnthropic,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":200}}`, 0},
		{"anthropic message_stop", providerTypeAnthropic, `{"type":"message_stop"}`, 0},
		{"vertex text part", providerTypeVertex,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello world!"}]}}]}`, 3},
		{"vertex parts summed", providerTypeVertex,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"abcdef"},{"text":"ghij"}]}}]}`, 2},
		{"vertex function call args", providerTypeVertex,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"q":"}x{","n":[1,2]}}}]}}]}`, 5},
		{"vertex text inside args counts once", providerTypeVertex,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"text":"abcd"}}}]}}]}`, 3},
		{"vertex final chunk still counts its text", providerTypeVertex,
			`{"candidates":[{"content":{"parts":[{"text":"abcdefgh"}]},"finishReason":"STOP"}],` +
				`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, out := estimateOf(t, tc.typ, "", tc.event); out != tc.want {
				t.Errorf("output estimate = %d, want %d", out, tc.want)
			}
		})
	}
}

// Output estimates add up across events.
func TestStreamEstimator_OutputAccumulates(t *testing.T) {
	_, out := estimateOf(t, providerTypeOpenAI, "",
		`{"choices":[{"delta":{"content":"hel"}}]}`,
		`{"choices":[{"delta":{"content":"aaaaaaaa"}}]}`,
		`[DONE]`)
	if out != 3 {
		t.Errorf("output estimate = %d, want 3", out)
	}
}

func TestJSONMembersLen(t *testing.T) {
	cases := []struct {
		name, data, member string
		want               int
	}{
		{"string", `{"text":"abc"}`, "text", 3},
		{"whitespace around the colon", `{"text" :  "abcd"}`, "text", 4},
		{"escapes count raw", `{"text":"a\"b\\"}`, "text", 6},
		{"every occurrence", `[{"text":"ab"},{"text":"cde"}]`, "text", 5},
		{"value equal to the name", `{"type":"text","text":"ab"}`, "text", 2},
		{"name inside a string value", `{"other":"say \"text\": \"nope\""}`, "text", 0},
		{"longer name", `{"text_delta":"abc","context":"x"}`, "text", 0},
		{"object to its matching brace", `{"args":{"a":{"b":"}"}},"x":1}`, "args", 15},
		{"array", `{"args":[1,[2]]}`, "args", 7},
		{"number", `{"text":12}`, "text", 0},
		{"null", `{"text":null}`, "text", 0},
		{"truncated string", `{"text":"abc`, "text", 0},
		{"truncated object", `{"args":{"a":"b"`, "args", 0},
		{"name at the end", `{"text"`, "text", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonMembersLen([]byte(tc.data), [][]byte{[]byte(tc.member)}); got != tc.want {
				t.Errorf("jsonMembersLen(%s, %s) = %d, want %d", tc.data, tc.member, got, tc.want)
			}
		})
	}
}

// fixedEstimator is a streamEstimator with a set estimate.
type fixedEstimator struct{ input, output int64 }

func (fixedEstimator) event([]byte)                      {}
func (f fixedEstimator) estimate() (input, output int64) { return f.input, f.output }

func TestWithEstimate(t *testing.T) {
	cases := []struct {
		name     string
		reported Usage
		est      streamEstimator
		want     Usage
	}{
		{"nil estimator", Usage{InputTokens: 7}, nil, Usage{InputTokens: 7}},
		{"fills both", Usage{}, fixedEstimator{10, 3},
			Usage{InputTokens: 10, OutputTokens: 3, InputEstimated: true, OutputEstimated: true}},
		{"fills only the missing part", Usage{InputTokens: 7}, fixedEstimator{10, 3},
			Usage{InputTokens: 7, OutputTokens: 3, OutputEstimated: true}},
		{"complete usage unchanged", Usage{InputTokens: 7, OutputTokens: 2}, fixedEstimator{10, 3},
			Usage{InputTokens: 7, OutputTokens: 2}},
		{"zero estimate leaves the part unmarked", Usage{InputTokens: 7}, fixedEstimator{10, 0},
			Usage{InputTokens: 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withEstimate(tc.reported, tc.est)
			if got.InputTokens != tc.want.InputTokens || got.OutputTokens != tc.want.OutputTokens ||
				got.InputEstimated != tc.want.InputEstimated || got.OutputEstimated != tc.want.OutputEstimated {
				t.Errorf("withEstimate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// estimatedPart keeps only the estimated counts, for pricing the estimate.
func TestUsage_EstimatedPart(t *testing.T) {
	u := Usage{InputTokens: 7, OutputTokens: 3, OutputEstimated: true, ServerTools: map[string]int64{"web_search": 1}}
	if p := u.estimatedPart(); p.InputTokens != 0 || p.OutputTokens != 3 || p.ServerTools != nil {
		t.Errorf("estimatedPart = %+v, want output 3 only", p)
	}
	if !u.estimated() || (Usage{InputTokens: 1}).estimated() {
		t.Error("estimated() must report a flagged part only")
	}
}
