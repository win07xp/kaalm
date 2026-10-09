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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// postRaw POSTs body as sent, with no re-encoding.
func postRaw(t *testing.T, c *http.Client, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// errorMessage decodes a gateway error and returns its type and message.
func errorMessage(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Error errorBody `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decoding error envelope %s: %v", raw, err)
	}
	return envelope.Error.Type, envelope.Error.Message
}

// Apart from the model value and the stream_options member the OpenAI
// adapter adds, the provider receives the agent's bytes: member order,
// whitespace, number text (an integer above 2^53 included), and HTML
// characters are kept.
func TestHandleLLMProxy_PassesTheBodyThroughExactly(t *testing.T) {
	sseUpstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	jsonUpstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	}
	for _, c := range []struct {
		name, providerType, path string
		upstream                 http.HandlerFunc
		body, want               string
	}{
		{"buffered", "openai", "/v1/chat/completions", jsonUpstream,
			`{ "model" : "prov/m1", "seed": 12345678901234567891, "temperature": 1.0, "messages":[{"role":"user","content":"<b>hi</b>"}] }`,
			`{ "model" : "m1", "seed": 12345678901234567891, "temperature": 1.0, "messages":[{"role":"user","content":"<b>hi</b>"}] }`},
		{"openai stream", "openai", "/v1/chat/completions", sseUpstream,
			`{"seed":12345678901234567891,"model":"prov/m1","stream":true,"messages":[]}`,
			`{"seed":12345678901234567891,"model":"m1","stream":true,"messages":[],"stream_options":{"include_usage":true}}`},
		{"anthropic stream", "anthropic", "/v1/messages", sseUpstream,
			`{"model":"prov/m1","stream":true,"max_tokens":16,"metadata":{"user_id":"<u>"},"messages":[]}`,
			`{"model":"m1","stream":true,"max_tokens":16,"metadata":{"user_id":"<u>"},"messages":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.upstream)
			h.seedRoute()
			h.store.providers["prov"].Spec.Type = c.providerType
			cert := agentCert(t, h.ca)
			resp := postRaw(t, h.client(&cert), h.url(c.path), []byte(c.body))
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := string((<-h.upreqs).raw); got != c.want {
				t.Errorf("upstream body:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// A body that names model, stream, or stream_options twice is refused: the
// gateway routes and meters by one reading, and a provider that kept the
// other copy could serve what the gateway never admitted.
func TestHandleLLMProxy_RejectsDuplicateRoutingFields(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a refused body must not reach the provider")
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	for _, c := range []struct{ body, field string }{
		{`{"model":"prov/m1","model":"prov/m1","messages":[]}`, "model"},
		{`{"model":"prov/m1","model":"other/m9","messages":[]}`, "model"},
		{`{"model":"prov/m1","stream":true,"stream":false,"messages":[]}`, "stream"},
		{`{"model":"prov/m1","stream":true,"stream_options":null,"stream_options":{"include_usage":false}}`, "stream_options"},
	} {
		resp := postRaw(t, client, h.url("/v1/chat/completions"), []byte(c.body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.body, resp.StatusCode)
		}
		typ, msg := errorMessage(t, resp)
		if want := `request body has more than one "` + c.field + `" field`; typ != errInvalidRequest || msg != want {
			t.Errorf("%s: error (%s, %q), want (invalid_request, %q)", c.body, typ, msg, want)
		}
	}
}

// The body checks keep their error messages.
func TestHandleLLMProxy_BodyShapeErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	const notJSON = "request body is not valid JSON"
	const noModel = `model must be a qualified "{providerRef}/{modelId}" name`
	for _, c := range []struct{ body, want string }{
		{`null`, noModel},
		{` null `, noModel},
		{`[]`, notJSON},
		{`"x"`, notJSON},
		{`1`, notJSON},
		{`true`, notJSON},
		{``, notJSON},
		{`{"model":"prov/m1"} x`, notJSON},
		{`{"model":"prov/m1"`, notJSON},
		{`{"model":7}`, noModel},
		{`{"model":null}`, noModel},
		{`{"Model":"prov/m1"}`, noModel},
		{`{}`, noModel},
	} {
		resp := postRaw(t, client, h.url("/v1/chat/completions"), []byte(c.body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", c.body, resp.StatusCode)
		}
		if typ, msg := errorMessage(t, resp); typ != errInvalidRequest || msg != c.want {
			t.Errorf("%q: error (%s, %q), want (invalid_request, %q)", c.body, typ, msg, c.want)
		}
	}
	// An escaped model value routes, as the decoded name.
	resp := postRaw(t, client, h.url("/v1/chat/completions"), []byte(`{"model":"prov\/m1","messages":[]}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("escaped model: status %d, want 200", resp.StatusCode)
	}
	if got := string((<-h.upreqs).raw); got != `{"model":"m1","messages":[]}` {
		t.Errorf("escaped model: upstream body %s", got)
	}
}

// A same-type fallback with a modelMap receives the primary's body with
// only the model changed.
func TestCandidateRequest_ModelMapRewritesOnlyTheModel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	backup := make(chan []byte, 1)
	h.addBackupProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		backup <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	h.store.providers["backup"].Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "m2"}}
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{
		Name: "backup", ModelMap: map[string]string{"m1": "m2"}}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
	cert := agentCert(t, h.ca)

	body := `{"seed":12345678901234567891, "model":"prov/m1","stream":true,"messages":[{"role":"user","content":"<hi>"}]}`
	resp := postRaw(t, h.client(&cert), h.url("/v1/chat/completions"), []byte(body))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	primary := string((<-h.upreqs).raw)
	got := string(<-backup)
	if want := strings.Replace(primary, `"m1"`, `"m2"`, 1); got != want {
		t.Errorf("backup body:\n got %s\nwant %s", got, want)
	}
}

func TestScanRequestFields(t *testing.T) {
	for _, c := range []struct {
		body             string
		model            string
		streamTrue, opts bool
		members          int
		err              string
	}{
		{body: `{}`},
		{body: ` { } `},
		{body: `null`},
		{body: `{"model":"p/m"}`, model: "p/m", members: 1},
		{body: `{ "model" : "p\/m" , "stream" : true }`, model: "p/m", streamTrue: true, members: 2},
		{body: `{"a":{"model":"x","stream":true},"b":["}",{"]":1}],"model":"p/m"}`, model: "p/m", members: 3},
		{body: `{"model":7,"stream":1,"stream_options":null}`, opts: true, members: 3},
		{body: `{"stream":"true","Model":"p/m"}`, members: 2},
		{body: `{"model":"p/m","s\"q":"\\\""}`, model: "p/m", members: 2},
		{body: `{"model":"p/m","model":"p/m"}`, err: `request body has more than one "model" field`},
		{body: `{"stream":false,"stream":true}`, err: `request body has more than one "stream" field`},
		{body: `{"stream_options":{},"stream_options":{}}`, err: `request body has more than one "stream_options" field`},
		{body: `[]`, err: errNotObject.Error()},
		{body: `"s"`, err: errNotObject.Error()},
	} {
		f, err := scanRequestFields([]byte(c.body))
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s: err = %v, want %q", c.body, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if got := f.model([]byte(c.body)); got != c.model || f.streamTrue != c.streamTrue ||
			f.hasStreamOptions != c.opts || f.members != c.members {
			t.Errorf("%s: model %q stream %v options %v members %d, want %q %v %v %d",
				c.body, got, f.streamTrue, f.hasStreamOptions, f.members, c.model, c.streamTrue, c.opts, c.members)
		}
	}
}

// decodeNumbers decodes b keeping numbers as their text.
func decodeNumbers(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// topLevelKeyCounts counts each top-level member name of a valid object.
func topLevelKeyCounts(t *testing.T, b []byte) map[string]int {
	dec := json.NewDecoder(bytes.NewReader(b))
	counts := map[string]int{}
	if _, err := dec.Token(); err != nil { // {
		t.Fatal(err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		counts[tok.(string)]++
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

// For any valid object, the scan reads what encoding/json reads, and the
// rewrite changes only the model value and the added member.
func FuzzScanRequestFields(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"model":"p/m","stream":true}`, `{"model":"p\/m","stream":true,"stream_options":null}`,
		`{"model":"x"}`, `{ "a" : [ 1 , { "b" : "}" } ] , "model" : "q" }`, `{"model":"a","model":"b"}`,
		`{"seed":12345678901234567891,"model":"m","messages":[{"content":"<b>\"x\"</b>"}]}`,
		"{\"mod\xffel\":1,\"model\":\"\xff\"}", `{"stream":true}`, `{"Stream":true,"STREAM_OPTIONS":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if !json.Valid(body) {
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return
		}
		counts := topLevelKeyCounts(t, body)
		fields, err := scanRequestFields(body)
		if counts["model"] > 1 || counts["stream"] > 1 || counts["stream_options"] > 1 {
			var dup *duplicateFieldError
			if !errors.As(err, &dup) {
				t.Fatalf("%q: err = %v, want a duplicate field error", body, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, n := range counts {
			total += n
		}
		if fields.members != total {
			t.Fatalf("%q: members %d, want %d", body, fields.members, total)
		}
		if raw, ok := m["model"]; ok != (fields.modelStart >= 0) ||
			(ok && string(body[fields.modelStart:fields.modelEnd]) != string(raw)) {
			t.Fatalf("%q: model span %d:%d, want %s", body, fields.modelStart, fields.modelEnd, raw)
		}
		var asAny map[string]any
		_ = json.Unmarshal(body, &asAny)
		wantModel, _ := asAny["model"].(string)
		if got := fields.model(body); got != wantModel {
			t.Fatalf("%q: model %q, want %q", body, got, wantModel)
		}
		if fields.streamTrue != (string(m["stream"]) == "true") {
			t.Fatalf("%q: streamTrue %v", body, fields.streamTrue)
		}
		if _, ok := m["stream_options"]; ok != fields.hasStreamOptions {
			t.Fatalf("%q: hasStreamOptions %v", body, fields.hasStreamOptions)
		}
		if body[fields.closeBrace] != '}' || len(bytes.TrimSpace(body[fields.closeBrace+1:])) != 0 {
			t.Fatalf("%q: closeBrace %d", body, fields.closeBrace)
		}
		for _, add := range []bool{false, true} {
			out := rewriteRequestBody(body, fields, "new/model<&>", add)
			if !json.Valid(out) {
				t.Fatalf("%q add=%v: invalid output %q", body, add, out)
			}
			got, err := decodeNumbers(out)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := decodeNumbers(body)
			wm := want.(map[string]any)
			if _, ok := wm["model"]; ok {
				wm["model"] = "new/model<&>"
			}
			if add {
				wm["stream_options"] = map[string]any{"include_usage": true}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%q add=%v: rewrite decodes to %v, want %v", body, add, got, want)
			}
		}
	})
}
