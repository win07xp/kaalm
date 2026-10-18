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

// Apart from the model value, the routing member names, and the
// stream_options member the OpenAI adapter adds, the provider receives the
// agent's bytes: member order, whitespace, number text (an integer above
// 2^53 included), and HTML characters are kept.
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
		{`{"model":"prov/m1","\u006dodel":"other/m9","messages":[]}`, "model"},
		{`{"model":"prov/m1","\u0073tream":true,"stream":false}`, "stream"},
		{`{"model":"prov/m1","stream":true,"stream":false,"messages":[]}`, "stream"},
		{`{"model":"prov/m1","stream":true,"stream_options":null,"stream_options":{"include_usage":false}}`, "stream_options"},
		{`{"model":"prov/m1","Model":"other/m9","messages":[]}`, "model"},
		{`{"MODEL":"other/m9","model":"prov/m1"}`, "model"},
		{`{"model":"prov/m1","stream":false,"Stream":true}`, "stream"},
		{`{"model":"prov/m1","\u017ftream":true,"stream":false}`, "stream"},
		{`{"model":"prov/m1","stream":true,"stream_options":{},"STREAM_OPTIONS":{"include_usage":false}}`, "stream_options"},
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

// A routing member written in another letter case is that member: the
// gateway routes by it and forwards it under its exact name, so an upstream
// that matches names exactly reads the member the gateway admitted.
func TestHandleLLMProxy_ForwardsRoutingNamesExactly(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	body := `{ "MODEL" : "prov/m1", "Stream":true,"seed":12345678901234567891,"messages":[]}`
	resp := postRaw(t, h.client(&cert), h.url("/v1/chat/completions"), []byte(body))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := `{ "model" : "m1", "stream":true,"seed":12345678901234567891,"messages":[],"stream_options":{"include_usage":true}}`
	if got := string((<-h.upreqs).raw); got != want {
		t.Errorf("upstream body:\n got %s\nwant %s", got, want)
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
		{`{"Model":7}`, noModel},
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
		{body: `{"stream":"true","Model":"p/m"}`, model: "p/m", members: 2},
		{body: `{"model":"p/m","s\"q":"\\\""}`, model: "p/m", members: 2},
		{body: `{"model":"p/m","model":"p/m"}`, err: `request body has more than one "model" field`},
		{body: `{"model":"p/m","\u006dodel":"q/n"}`, err: `request body has more than one "model" field`},
		{body: `{"\u0073tream":true,"stream":false}`, err: `request body has more than one "stream" field`},
		{body: `{"\u006dodel":"p/m","stream":true}`, model: "p/m", streamTrue: true, members: 2},
		{body: `{"stream":false,"stream":true}`, err: `request body has more than one "stream" field`},
		{body: `{"stream_options":{},"stream_options":{}}`, err: `request body has more than one "stream_options" field`},
		{body: `{"model":"p/m","Model":"q/n"}`, err: `request body has more than one "model" field`},
		{body: `{"MODEL":"p/m","model":"p/m"}`, err: `request body has more than one "model" field`},
		{body: `{"Stream":true,"stream":false}`, err: `request body has more than one "stream" field`},
		{body: `{"\u017ftream":true,"stream":false}`, err: `request body has more than one "stream" field`},
		{body: `{"ſtream":true,"stream":false}`, err: `request body has more than one "stream" field`},
		{body: `{"stream_options":{},"Stream_Options":null}`, err: `request body has more than one "stream_options" field`},
		{body: `{"Model":"p/m","STREAM":true,"Stream_Options":null}`, model: "p/m", streamTrue: true, opts: true, members: 3},
		{body: `{"\u017ftream":true}`, streamTrue: true, members: 1},
		{body: `{"mode":"x","models":"y","streams":true}`, members: 3},
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

// The rewrite writes each routing member's name exactly and keeps every
// other byte; a body that already uses the exact names and the same model
// comes back unchanged.
func TestRewriteRequestBody_WritesRoutingNamesExactly(t *testing.T) {
	body := []byte(`{"x":1,"\u0073tream" : false,"Model":"p/m"}`)
	f, err := scanRequestFields(body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(rewriteRequestBody(body, f, "n", false)), `{"x":1,"stream" : false,"model":"n"}`; got != want {
		t.Errorf("rewrite:\n got %s\nwant %s", got, want)
	}
	all := []byte(`{"Stream_Options":null,"Model":"p/m","STREAM":true}`)
	if f, err = scanRequestFields(all); err != nil {
		t.Fatal(err)
	}
	want := `{"stream_options":null,"model":"n","stream":true,"stream_options":{"include_usage":true}}`
	if got := string(rewriteRequestBody(all, f, "n", true)); got != want {
		t.Errorf("every edit:\n got %s\nwant %s", got, want)
	}
	exact := []byte(`{"stream":false, "model":"n","stream_options":null}`)
	if f, err = scanRequestFields(exact); err != nil {
		t.Fatal(err)
	}
	if got := rewriteRequestBody(exact, f, "n", false); !bytes.Equal(got, exact) {
		t.Errorf("exact names: got %s, want the body unchanged", got)
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

// topLevelKeyCounts counts each top-level member name of a valid object. A
// name that matches a routing name the way encoding/json matches struct
// fields counts under the routing name.
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
		counts[canonicalRoutingName(tok.(string))]++
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

// canonicalRoutingName returns the routing name that name matches, or name.
func canonicalRoutingName(name string) string {
	for _, r := range routingNames {
		if jsonNameMatches(name, r) {
			return r
		}
	}
	return name
}

// routingMembers is what encoding/json reads into a struct with the routing
// fields; a member that is present holds at least "null".
type routingMembers struct {
	Model         json.RawMessage `json:"model"`
	Stream        json.RawMessage `json:"stream"`
	StreamOptions json.RawMessage `json:"stream_options"`
}

// checkRewrite checks that the rewrite of a valid object decodes to the
// object with each routing member under its exact name, the model replaced,
// and, with add, the gateway's stream_options.
func checkRewrite(t *testing.T, body []byte, fields requestFields, add bool) {
	t.Helper()
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
	for k, v := range wm {
		if r := canonicalRoutingName(k); r != k {
			delete(wm, k)
			wm[r] = v
		}
	}
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

// For any valid object, the scan reads what encoding/json reads, and the
// rewrite changes only the routing names, the model value, and the added
// member.
func FuzzScanRequestFields(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"model":"p/m","stream":true}`, `{"model":"p\/m","stream":true,"stream_options":null}`,
		`{"model":"x"}`, `{ "a" : [ 1 , { "b" : "}" } ] , "model" : "q" }`, `{"model":"a","model":"b"}`,
		`{"model":"a","\u006dodel":"b"}`, `{"\u006dodel":"a","\u0073tream":true}`,
		`{"seed":12345678901234567891,"model":"m","messages":[{"content":"<b>\"x\"</b>"}]}`,
		"{\"mod\xffel\":1,\"model\":\"\xff\"}", `{"stream":true}`, `{"Stream":true,"STREAM_OPTIONS":1}`,
		`{"Model":"a","model":"b"}`, `{"\u017ftream":true}`, `{"MODEL":"x","Stream_Options":null}`,
		"{\"\u017ftream\":true,\"model\":\"m\"}",
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
		var rm routingMembers
		if err := json.Unmarshal(body, &rm); err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, n := range counts {
			total += n
		}
		if fields.members != total {
			t.Fatalf("%q: members %d, want %d", body, fields.members, total)
		}
		if ok := len(rm.Model) > 0; ok != (fields.modelStart >= 0) ||
			(ok && string(body[fields.modelStart:fields.modelEnd]) != string(rm.Model)) {
			t.Fatalf("%q: model span %d:%d, want %s", body, fields.modelStart, fields.modelEnd, rm.Model)
		}
		var wantModel string
		if len(rm.Model) > 0 && rm.Model[0] == '"' {
			_ = json.Unmarshal(rm.Model, &wantModel)
		}
		if got := fields.model(body); got != wantModel {
			t.Fatalf("%q: model %q, want %q", body, got, wantModel)
		}
		if fields.streamTrue != (string(rm.Stream) == "true") {
			t.Fatalf("%q: streamTrue %v", body, fields.streamTrue)
		}
		if ok := len(rm.StreamOptions) > 0; ok != fields.hasStreamOptions {
			t.Fatalf("%q: hasStreamOptions %v", body, fields.hasStreamOptions)
		}
		if body[fields.closeBrace] != '}' || len(bytes.TrimSpace(body[fields.closeBrace+1:])) != 0 {
			t.Fatalf("%q: closeBrace %d", body, fields.closeBrace)
		}
		for _, add := range []bool{false, true} {
			checkRewrite(t, body, fields, add)
		}
	})
}
