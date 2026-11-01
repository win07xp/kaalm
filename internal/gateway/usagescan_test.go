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
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// usageOf reads usage from a whole response body with the adapter's
// streaming reader, the way the relay does in one write.
func usageOf(a providerAdapter, body []byte) (Usage, bool) {
	r := a.usageReader()
	_, _ = r.Write(body)
	return r.result()
}

// The oracles below are the whole-body decodes the relay's reader replaced.
// The reader must agree with them on well-formed answers.

func oracleAnthropicUsage(body []byte) (Usage, bool) {
	var resp struct {
		Usage struct {
			InputTokens   int64           `json:"input_tokens"`
			OutputTokens  int64           `json:"output_tokens"`
			ServerToolUse json.RawMessage `json:"server_tool_use"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	u := Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
		ServerTools: serverToolCounts(resp.Usage.ServerToolUse)}
	if u.isZero() {
		return Usage{}, false
	}
	return u, true
}

func oracleOpenAIUsage(body []byte) (Usage, bool) {
	var resp struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	u := Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	if u.isZero() {
		return Usage{}, false
	}
	return u, true
}

func oracleVertexUsage(body []byte) (Usage, bool) {
	var resp struct {
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
		Candidates []vertexCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	u := Usage{InputTokens: resp.UsageMetadata.PromptTokenCount,
		OutputTokens: resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount,
		ServerTools:  vertexServerTools(resp.Candidates)}
	if u.isZero() {
		return Usage{}, false
	}
	return u, true
}

var usageCorpus = []struct {
	name    string
	adapter providerAdapter
	oracle  func([]byte) (Usage, bool)
	body    string
}{
	{"openai canned", openaiAdapter{}, oracleOpenAIUsage, string(openAIResponse)},
	{"openai usage first", openaiAdapter{}, oracleOpenAIUsage,
		`{"usage":{"prompt_tokens":3,"completion_tokens":5},"choices":[{"message":{"content":"a \"usage\" b \\ c"}}]}`},
	{"openai nested usage ignored", openaiAdapter{}, oracleOpenAIUsage,
		`{"choices":[{"message":{"content":"x","usage":{"prompt_tokens":99}}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`},
	{"openai no usage", openaiAdapter{}, oracleOpenAIUsage, `{"choices":[]}`},
	{"openai zero usage", openaiAdapter{}, oracleOpenAIUsage, `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`},
	{"openai null usage", openaiAdapter{}, oracleOpenAIUsage, `{"usage":null,"id":"x"}`},
	{"openai not json", openaiAdapter{}, oracleOpenAIUsage, `not json`},
	{"openai empty object", openaiAdapter{}, oracleOpenAIUsage, `{}`},
	{"openai pretty", openaiAdapter{}, oracleOpenAIUsage,
		"{\n  \"id\" : \"x\",\n  \"usage\" : {\n    \"prompt_tokens\" : 7 ,\n    \"completion_tokens\" : 8\n  }\n}\n"},
	{"anthropic plain", anthropicAdapter{}, oracleAnthropicUsage,
		`{"id":"msg","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":25}}`},
	{"anthropic server tools", anthropicAdapter{}, oracleAnthropicUsage,
		`{"content":[{"type":"server_tool_use","name":"web_search"}],"usage":{"input_tokens":10,"output_tokens":4,"server_tool_use":{"web_search_requests":2}}}`},
	{"anthropic server tools only", anthropicAdapter{}, oracleAnthropicUsage,
		`{"usage":{"server_tool_use":{"web_search_requests":1}}}`},
	{"anthropic no usage", anthropicAdapter{}, oracleAnthropicUsage, `{"content":[]}`},
	{"anthropic not json", anthropicAdapter{}, oracleAnthropicUsage, `not json`},
	{"vertex thoughts", vertexAdapter{}, oracleVertexUsage,
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12,"thoughtsTokenCount":40}}`},
	{"vertex two grounded candidates", vertexAdapter{}, oracleVertexUsage,
		`{"candidates":[{"groundingMetadata":{"webSearchQueries":["a","b"]}},{"content":{},"groundingMetadata":{"webSearchQueries":["c"]}}],` +
			`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`},
	{"vertex grounding without queries", vertexAdapter{}, oracleVertexUsage,
		`{"candidates":[{}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`},
	{"vertex no usage", vertexAdapter{}, oracleVertexUsage, `{"candidates":[]}`},
	{"vertex not json", vertexAdapter{}, oracleVertexUsage, `not json`},
	{"vertex empty object", vertexAdapter{}, oracleVertexUsage, `{}`},
}

func TestUsageReader_MatchesTheWholeBodyDecode(t *testing.T) {
	for _, tc := range usageCorpus {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := tc.oracle([]byte(tc.body))
			check := func(how string, got Usage, ok bool) {
				t.Helper()
				if ok != wantOK || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: got %+v, %v; want %+v, %v", how, got, ok, want, wantOK)
				}
			}
			got, ok := usageOf(tc.adapter, []byte(tc.body))
			check("whole", got, ok)
			for split := 0; split <= len(tc.body); split++ {
				r := tc.adapter.usageReader()
				_, _ = r.Write([]byte(tc.body[:split]))
				_, _ = r.Write([]byte(tc.body[split:]))
				got, ok := r.result()
				check("split", got, ok)
			}
			r := tc.adapter.usageReader()
			for i := 0; i < len(tc.body); i++ {
				_, _ = r.Write([]byte{tc.body[i]})
			}
			got, ok = r.result()
			check("byte by byte", got, ok)
		})
	}
}

func TestUsageScanner_EscapesInsideStrings(t *testing.T) {
	body := `{"text":"a \" } ] { [ \\\" \\","usage":{"prompt_tokens":4,"completion_tokens":2}}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); !ok || got.InputTokens != 4 || got.OutputTokens != 2 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
}

func TestUsageScanner_UsageInsideAStringIsIgnored(t *testing.T) {
	body := `{"choices":[{"message":{"content":"{\"usage\":{\"prompt_tokens\":9}}"}}]}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); ok {
		t.Fatalf("usage = %+v from a content string", got)
	}
}

func TestUsageScanner_EscapedKeyDoesNotMatch(t *testing.T) {
	// The relay matches member names by exact spelling.
	body := `{"` + `\` + `u0075sage":{"prompt_tokens":9,"completion_tokens":1}}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); ok {
		t.Fatalf("usage = %+v from an escaped key", got)
	}
}

func TestUsageScanner_DuplicateUsageLastWins(t *testing.T) {
	body := `{"usage":{"prompt_tokens":1,"completion_tokens":1},"x":1,"usage":{"prompt_tokens":5,"completion_tokens":6}}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); !ok || got.InputTokens != 5 || got.OutputTokens != 6 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
}

func TestUsageScanner_OverCapValueIsDropped(t *testing.T) {
	body := `{"usage":{"prompt_tokens":1,"completion_tokens":1,"pad":"` + strings.Repeat("x", usageCaptureLimit) + `"}}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); ok {
		t.Fatalf("usage = %+v from a value over the capture cap", got)
	}
}

func TestUsageScanner_LongKeyMatchesNothing(t *testing.T) {
	body := `{"` + strings.Repeat("k", 200) + `":{"usage":1},"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); !ok || got.InputTokens != 3 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
}

func TestUsageScanner_TopLevelArrayYieldsNothing(t *testing.T) {
	body := `[{"usage":{"prompt_tokens":3,"completion_tokens":1}}]`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); ok {
		t.Fatalf("usage = %+v from a top-level array", got)
	}
}

func TestUsageScanner_LargeStringKeepsTheCaptureSmall(t *testing.T) {
	r := openaiAdapter{}.usageReader()
	_, _ = r.Write([]byte(`{"choices":[{"message":{"content":"`))
	chunk := []byte(strings.Repeat("y", 64<<10))
	for range 64 { // 4 MiB of answer text
		_, _ = r.Write(chunk)
		if c := cap(r.scan.capBuf); c > usageCaptureLimit*2 {
			t.Fatalf("capture buffer grew to %d bytes", c)
		}
	}
	_, _ = r.Write([]byte(`"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	if got, ok := r.result(); !ok || got.OutputTokens != 2 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
	if c := cap(r.scan.capBuf); c > usageCaptureLimit*2 {
		t.Fatalf("capture buffer grew to %d bytes", c)
	}
}

func TestUsageScanner_CutBodyYieldsWhatWasSeen(t *testing.T) {
	body := `{"usage":{"prompt_tokens":3,"completion_tokens":2},"choices":[{"message":{"content":"half`
	if got, ok := usageOf(openaiAdapter{}, []byte(body)); !ok || got.InputTokens != 3 || got.OutputTokens != 2 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
	cutInsideUsage := `{"usage":{"prompt_tokens":3,"completion_`
	if got, ok := usageOf(openaiAdapter{}, []byte(cutInsideUsage)); ok {
		t.Fatalf("usage = %+v from a cut usage member", got)
	}
}

func TestUsageScanner_ScalarAtAPath(t *testing.T) {
	var got []string
	s := newUsageScanner(func(path int, v []byte) { got = append(got, string(v)) }, jsonPath{"a", "b"})
	_, _ = s.Write([]byte(`{"a":{"b":12.5e3,"c":true},"a":{"b": "x\"y" },"b":1}`))
	want := []string{"12.5e3", `"x\"y"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured %q, want %q", got, want)
	}
}
