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
	"fmt"
	"strings"
	"testing"
	"time"
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
	return est.estimateInput(), est.estimateOutput()
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

// The input estimate leaves out each adapter's images, audio, and video, so
// a request is estimated by its text alone.
func TestStreamEstimator_InputLeavesOutMedia(t *testing.T) {
	b64 := strings.Repeat("iVBORw0KGgo", 1000)
	dataURL := "data:image/png;base64," + b64
	cases := []struct {
		name, typ, template, media string
	}{
		{"openai image_url data URL", providerTypeOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
				`{"type":"image_url","image_url":{"url":"%s","detail":"auto"}}]}],"stream":true}`, dataURL},
		{"openai responses input_image", providerTypeOpenAI,
			`{"model":"gpt-4o","input":[{"role":"user","content":[{"type":"input_text","text":"what is this"},` +
				`{"type":"input_image","image_url":"%s"}]}],"stream":true}`, dataURL},
		{"openai input_audio", providerTypeOpenAI,
			`{"model":"gpt-4o-audio","messages":[{"role":"user","content":[{"type":"text","text":"transcribe"},` +
				`{"type":"input_audio","input_audio":{"data":"%s","format":"wav"}}]}],"stream":true}`, b64},
		{"openai video_url data URL", providerTypeOpenAICompatible,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what happens"},` +
				`{"type":"video_url","video_url":{"url":"%s"}}]}],"stream":true}`, "data:video/mp4;base64," + b64},
		{"openai file part holding an image", providerTypeOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
				`{"type":"file","file":{"filename":"a.png","file_data":"%s"}}]}],"stream":true}`, dataURL},
		{"anthropic image source", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}},` +
				`{"type":"text","text":"what is this"}]}],"stream":true}`, b64},
		{"anthropic image source with the type after the data", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"image","source":{"type":"base64","data":"%s","media_type":"image/jpeg"}},` +
				`{"type":"text","text":"what is this"}]}],"stream":true}`, b64},
		{"vertex inlineData", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"what is this"},` +
				`{"inlineData":{"mimeType":"image/png","data":"%s"}}]}]}`, b64},
		{"vertex inline_data", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"what is this"},` +
				`{"inline_data":{"mime_type":"image/png","data":"%s"}}]}]}`, b64},
		{"vertex inlineData audio", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"transcribe"},` +
				`{"inlineData":{"mimeType":"audio/mpeg","data":"%s"}}]}]}`, b64},
		{"vertex inline_data video", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"what happens"},` +
				`{"inline_data":{"mime_type":"video/mp4","data":"%s"}}]}]}`, b64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Sprintf(tc.template, tc.media)
			want := int64((len(request) - len(tc.media)) / bytesPerToken)
			if in, _ := estimateOf(t, tc.typ, request); in != want {
				t.Errorf("input estimate = %d, want %d (the request without its media)", in, want)
			}
		})
	}
}

// The input estimate counts documents (PDF and text) and values whose media
// type is missing: the whole request over four.
func TestStreamEstimator_InputCountsDocuments(t *testing.T) {
	b64 := strings.Repeat("JVBERi0xLjQK", 1000)
	cases := []struct {
		name, typ, template, value string
	}{
		{"openai file part", providerTypeOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"summarize"},` +
				`{"type":"file","file":{"filename":"a.pdf","file_data":"%s"}}]}],"stream":true}`,
			"data:application/pdf;base64," + b64},
		{"openai file_data without a data URL", providerTypeOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"summarize"},` +
				`{"type":"file","file":{"filename":"a.pdf","file_data":"%s"}}]}],"stream":true}`, b64},
		{"anthropic document source", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"%s"}},` +
				`{"type":"text","text":"summarize"}]}],"stream":true}`, b64},
		{"anthropic text document source", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"%s"}},` +
				`{"type":"text","text":"summarize"}]}],"stream":true}`, strings.Repeat("plain document text ", 500)},
		{"anthropic image source without a media type", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"image","source":{"type":"base64","data":"%s"}}]}],"stream":true}`, b64},
		{"vertex inlineData text", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"summarize"},` +
				`{"inlineData":{"mimeType":"text/plain","data":"%s"}}]}]}`, b64},
		{"vertex inlineData pdf", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"text":"summarize"},` +
				`{"inlineData":{"mimeType":"application/pdf","data":"%s"}}]}]}`, b64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Sprintf(tc.template, tc.value)
			want := int64(len(request) / bytesPerToken)
			if in, _ := estimateOf(t, tc.typ, request); in != want {
				t.Errorf("input estimate = %d, want %d (the whole request)", in, want)
			}
		})
	}
}

// A member named data outside a media member is text, and counts: tool
// results, tool inputs, and other request members.
func TestStreamEstimator_InputCountsDataOutsideMedia(t *testing.T) {
	text := strings.Repeat("plain tool output ", 500)
	cases := []struct {
		name, typ, template string
	}{
		{"vertex functionResponse", providerTypeVertex,
			`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f",` +
				`"response":{"status":"ok","data":"%s"}}}]}]}`},
		{"vertex functionCall args", providerTypeVertex,
			`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"data":"%s"}}}]}]}`},
		{"anthropic tool_use input", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"t1","name":"f","input":{"data":"%s"}}]}],"stream":true}`},
		{"anthropic tool_result object", providerTypeAnthropic,
			`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"x"}],"data":"%s"}]}],"stream":true}`},
		{"openai metadata", providerTypeOpenAI,
			`{"model":"gpt-4o","metadata":{"data":"%s"},"messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"openai tool message", providerTypeOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"tool","tool_call_id":"c1","content":"{\"data\":\"%s\"}"}],"stream":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := fmt.Sprintf(tc.template, text)
			want := int64(len(request) / bytesPerToken)
			if in, _ := estimateOf(t, tc.typ, request); in != want {
				t.Errorf("input estimate = %d, want %d (the whole request)", in, want)
			}
		})
	}
}

// An image given by an https URL is text in the request, and counts.
func TestStreamEstimator_InputCountsRemoteImageURL(t *testing.T) {
	request := `{"model":"gpt-4o","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/` + strings.Repeat("a", 400) + `.png"}}]}]}`
	if in, _ := estimateOf(t, providerTypeOpenAI, request); in != int64(len(request)/bytesPerToken) {
		t.Errorf("input estimate = %d, want %d", in, len(request)/bytesPerToken)
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
		{"openai legacy completions text", providerTypeOpenAI,
			`{"id":"cmpl-1","object":"text_completion","choices":[{"text":"some generated text here","index":0,` +
				`"logprobs":null,"finish_reason":null}]}`, 6},
		{"openai chat logprobs not counted", providerTypeOpenAI,
			`{"choices":[{"index":0,"delta":{"content":"Hi"},"logprobs":{"content":[{"token":"Hi","logprob":-0.1,` +
				`"bytes":[72,105],"top_logprobs":[{"token":"Hi","logprob":-0.1,"bytes":[72,105]},` +
				`{"token":"Hello","logprob":-2.3,"bytes":[72,101,108,108,111]},` +
				`{"token":"Hey","logprob":-3.1,"bytes":[72,101,121]}]}]},"finish_reason":null}]}`, 1},
		{"openai legacy logprobs not counted", providerTypeOpenAI,
			`{"choices":[{"text":"abcd","index":0,"logprobs":{"tokens":["abcd"],"token_logprobs":[-0.1],` +
				`"top_logprobs":[{"abcd":-0.1,"text":-2.0}],"text_offset":[0]},"finish_reason":null}]}`, 1},
		{"openai logprobs after many spaces not counted", providerTypeOpenAI,
			`{"choices":[{"index":0,"delta":{"content":"Hi"},"logprobs":` + strings.Repeat(" ", 80) +
				`{"content":[{"token":"Hi","logprob":-0.1,"bytes":[72,105]}]},"finish_reason":null}]}`, 1},
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
		{"whitespace before an object counted once", `{"args":` + strings.Repeat(" ", 40) +
			`{"args":{"a":"bcdefghijklmnop"}},"x":1}`, "args", 32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonMembersLen([]byte(tc.data), [][]byte{[]byte(tc.member)}); got != tc.want {
				t.Errorf("jsonMembersLen(%s, %s) = %d, want %d", tc.data, tc.member, got, tc.want)
			}
		})
	}
}

// mediaLen counts a parent rule's string member only at the parent
// object's own level.
func TestMediaLen_ParentRule(t *testing.T) {
	media := []mediaMember{{parent: []byte("source"), name: []byte("data"), kind: mediaAlways}}
	cases := []struct {
		name, obj string
		want      int
	}{
		{"string member", `{"mimeType":"image/png","data":"abc"}`, 3},
		{"after scalars", `{"n":1, "ok":true,"x":null , "data" : "abc"}`, 3},
		{"after an object and an array", `{"o":{"data":"no"},"a":["data"],"data":"abc"}`, 3},
		{"nested only", `{"o":{"data":"no"}}`, 0},
		{"not a string", `{"data":{"a":"b"}}`, 0},
		{"missing", `{"mimeType":"image/png"}`, 0},
		{"empty object", `{}`, 0},
		{"not an object", `"data"`, 0},
		{"an array", `[{"data":"no"}]`, 0},
		{"escaped quotes", `{"k":"a\\\"b:","data":"a\\\"bc"}`, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"source":` + tc.obj + `,"data":"text"}`
			if got := mediaLen([]byte(body), media); got != tc.want {
				t.Errorf("mediaLen(%s) = %d, want %d", body, got, tc.want)
			}
		})
	}
}

// mediaLen leaves out a typed member's value only when the type member
// beside it, before or after it in the same object, names an image, audio,
// or video type. Every copy of the value is summed, the last copy of the
// type member decides, and a type that is missing, not a string, or another
// type counts the value.
func TestMediaLen_TypeRule(t *testing.T) {
	cases := []struct {
		name, obj string
		want      int
	}{
		{"type before data", `{"type":"base64","media_type":"image/png","data":"abc"}`, 3},
		{"type after data", `{"data":"abc","media_type":"image/png"}`, 3},
		{"upper case type", `{"media_type":"IMAGE/PNG","data":"abc"}`, 3},
		{"type with parameters", `{"media_type":"image/png;base64","data":"abc"}`, 3},
		{"audio", `{"media_type":"audio/wav","data":"abc"}`, 3},
		{"video", `{"media_type":"video/mp4","data":"abc"}`, 3},
		{"pdf", `{"media_type":"application/pdf","data":"abc"}`, 0},
		{"text", `{"type":"text","media_type":"text/plain","data":"abc"}`, 0},
		{"no slash", `{"media_type":"image","data":"abc"}`, 0},
		{"leading space", `{"media_type":" image/png","data":"abc"}`, 0},
		{"escaped slash", `{"media_type":"image\/png","data":"abc"}`, 0},
		{"missing type", `{"type":"base64","data":"abc"}`, 0},
		{"number type", `{"media_type":5,"data":"abc"}`, 0},
		{"object type", `{"media_type":{"a":1},"data":"abc"}`, 0},
		{"array type", `{"media_type":["image/png"],"data":"abc"}`, 0},
		{"null type", `{"data":"abc","media_type":null}`, 0},
		{"last type copy wins, not media", `{"media_type":"image/png","data":"abc","media_type":"application/pdf"}`, 0},
		{"last type copy wins, media", `{"media_type":"application/pdf","data":"abc","media_type":"image/png"}`, 3},
		{"non-string last type copy", `{"media_type":"image/png","data":"abc","media_type":null}`, 0},
		{"data copies summed", `{"media_type":"image/png","data":"abc","data":"de"}`, 5},
		{"type in a nested object", `{"data":"abc","n":{"media_type":"image/png"}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"source":` + tc.obj + `,"data":"text"}`
			if got := mediaLen([]byte(body), anthropicMediaMembers); got != tc.want {
				t.Errorf("mediaLen(%s) = %d, want %d", body, got, tc.want)
			}
		})
	}
	t.Run("type in the parent object", func(t *testing.T) {
		body := `{"media_type":"image/png","source":{"data":"abc"}}`
		if got := mediaLen([]byte(body), anthropicMediaMembers); got != 0 {
			t.Errorf("mediaLen(%s) = %d, want 0", body, got)
		}
	})
	t.Run("body cut off inside the data", func(t *testing.T) {
		body := `{"source":{"media_type":"image/png","data":"abc`
		if got := mediaLen([]byte(body), anthropicMediaMembers); got != 0 {
			t.Errorf("mediaLen(%s) = %d, want 0", body, got)
		}
	})
	t.Run("body cut off after the data", func(t *testing.T) {
		body := `{"source":{"media_type":"image/png","data":"abc",`
		if got := mediaLen([]byte(body), anthropicMediaMembers); got != 0 {
			t.Errorf("mediaLen(%s) = %d, want 0", body, got)
		}
	})
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"vertex inlineData video", `{"inlineData":{"data":"abc","mimeType":"video/mp4"}}`, 3},
		{"vertex inline_data text", `{"inline_data":{"mime_type":"text/plain","data":"abc"}}`, 0},
		{"vertex camel type on the snake member", `{"inline_data":{"mimeType":"image/png","data":"abc"}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediaLen([]byte(tc.body), vertexMediaMembers); got != tc.want {
				t.Errorf("mediaLen(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// mediaLen leaves out a data URL only when the type after data: is an
// image, audio, or video type; input_audio data is always audio.
func TestMediaLen_DataURL(t *testing.T) {
	cases := []struct {
		name, value string
		media       bool
	}{
		{"image", "data:image/png;base64,abc", true},
		{"upper case type", "data:IMAGE/PNG;base64,abc", true},
		{"video", "data:video/mp4;base64,abc", true},
		{"pdf", "data:application/pdf;base64,abc", false},
		{"no type", "data:;base64,abc", false},
		{"upper case scheme", "DATA:image/png;base64,abc", false},
		{"remote URL", "https://example.com/a.png", false},
		{"raw base64", "iVBORw0KGgo", false},
	}
	for _, member := range []string{"url", "image_url", "file_data"} {
		for _, tc := range cases {
			t.Run(member+" "+tc.name, func(t *testing.T) {
				body := `{"part":{"` + member + `":"` + tc.value + `"},"text":"x"}`
				want := 0
				if tc.media {
					want = len(tc.value)
				}
				if got := mediaLen([]byte(body), openaiMediaMembers); got != want {
					t.Errorf("mediaLen(%s) = %d, want %d", body, got, want)
				}
			})
		}
	}
	body := `{"input_audio":{"format":"wav","data":"abc"}}`
	if got := mediaLen([]byte(body), openaiMediaMembers); got != 3 {
		t.Errorf("mediaLen(%s) = %d, want 3", body, got)
	}
}

// mediaLen decides a typed object once, when it closes, so an object with
// many data members costs one pass.
func TestMediaLen_ManySiblingsIsOnePass(t *testing.T) {
	const copies = 200_000
	body := `{"source":{"media_type":"image/png",` + strings.Repeat(`"data":"abcdefgh",`, copies) + `"data":"abcdefgh"}}`
	start := time.Now()
	if got, want := mediaLen([]byte(body), anthropicMediaMembers), 8*(copies+1); got != want {
		t.Errorf("mediaLen = %d, want %d", got, want)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("mediaLen took %s on %d data members, want under a second", took, copies+1)
	}
}

// mediaLen walks a body once, so a deep chain of objects under a media
// member's name costs no more than a flat body of the same size.
func TestMediaLen_DeepNestingIsOnePass(t *testing.T) {
	const depth = 3000
	filler := `[` + strings.Repeat(`1,`, 500_000) + `1]`
	rules := []struct {
		name  string
		media []mediaMember
	}{
		{"source", anthropicMediaMembers},
		{"inlineData", vertexMediaMembers},
		{"input_audio", openaiMediaMembers},
		{"image_url", openaiMediaMembers},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			body := `{"x":` + strings.Repeat(`{"`+r.name+`":`, depth) + `{"t":` + filler + `}` +
				strings.Repeat(`}`, depth) + `}`
			start := time.Now()
			if got := mediaLen([]byte(body), r.media); got != 0 {
				t.Errorf("mediaLen = %d, want 0", got)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("mediaLen took %s on a %d byte body nested %d deep, want under a second", took, len(body), depth)
			}
		})
	}
}

// fixedEstimator is a streamEstimator with a set estimate.
type fixedEstimator struct{ input, output int64 }

func (fixedEstimator) event([]byte)            {}
func (f fixedEstimator) estimateInput() int64  { return f.input }
func (f fixedEstimator) estimateOutput() int64 { return f.output }

// inputCountingEstimator counts the calls for an input estimate.
type inputCountingEstimator struct {
	fixedEstimator
	inputCalls int
}

func (e *inputCountingEstimator) estimateInput() int64 {
	e.inputCalls++
	return e.input
}

// withEstimate asks for an input estimate only when the provider reported no
// input, so a stream with reported input pays no scan of its request.
func TestWithEstimate_InputEstimatedOnlyWhenMissing(t *testing.T) {
	est := &inputCountingEstimator{fixedEstimator: fixedEstimator{10, 3}}
	withEstimate(Usage{InputTokens: 7}, est)
	if est.inputCalls != 0 {
		t.Errorf("input estimated %d times for a stream with reported input, want 0", est.inputCalls)
	}
	withEstimate(Usage{OutputTokens: 2}, est)
	if est.inputCalls != 1 {
		t.Errorf("input estimated %d times for a stream with no reported input, want 1", est.inputCalls)
	}
}

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
