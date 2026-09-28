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

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// vertexFake is one TLS server playing both Google's token endpoint and the
// Vertex publisher-models endpoint. Each response status is settable.
type vertexFake struct {
	mu                        sync.Mutex
	tokenStatus, modelsStatus int
	tokenBody                 string
	modelsPath, modelsAuth    string
	srv                       *httptest.Server
}

func newVertexFake(t *testing.T) *vertexFake {
	f := &vertexFake{tokenStatus: http.StatusOK, modelsStatus: http.StatusOK,
		tokenBody: `{"access_token":"ya29.probe","token_type":"Bearer","expires_in":3600}`}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/token" {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenBody))
			return
		}
		f.modelsPath, f.modelsAuth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(f.modelsStatus)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *vertexFake) set(token, models int, tokenBody string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenStatus, f.modelsStatus = token, models
	if tokenBody != "" {
		f.tokenBody = tokenBody
	}
}

func vertexKey(t *testing.T, tokenURI string) string {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "my-proj",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "probe@my-proj.iam.gserviceaccount.com", "token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The Vertex probe mints an OAuth2 token from the service-account key and
// lists publisher models with it. A rejected key, at the token endpoint or
// at Vertex, is AuthFailed; other failures are transient.
func TestHTTPProbe_Vertex(t *testing.T) {
	f := newVertexFake(t)
	key := vertexKey(t, f.srv.URL+"/token")
	checker := &HTTPProviderHealthChecker{Client: f.srv.Client()}
	probe := func(credential string) ProviderProbeResult {
		return checker.Probe(context.Background(), &kaalmv1beta1.ModelProvider{
			Spec: kaalmv1beta1.ModelProviderSpec{Type: "google-vertex", Endpoint: f.srv.URL + "/"},
		}, credential)
	}

	if res := probe(key); !res.Healthy {
		t.Fatalf("token and models 200: %+v, want Healthy", res)
	}
	if want := "/v1/projects/my-proj/locations/global/publishers/google/models"; f.modelsPath != want {
		t.Errorf("probed %q, want %q", f.modelsPath, want)
	}
	if f.modelsAuth != "Bearer ya29.probe" {
		t.Errorf("Authorization = %q, want the minted bearer token", f.modelsAuth)
	}

	cases := []struct {
		name                string
		token, models       int
		tokenBody           string
		wantAuth, wantError bool
	}{
		{"models 403", http.StatusOK, http.StatusForbidden, "", true, false},
		{"models 401", http.StatusOK, http.StatusUnauthorized, "", true, false},
		{"models 503", http.StatusOK, http.StatusServiceUnavailable, "", false, true},
		{"token invalid_grant", http.StatusBadRequest, http.StatusOK, `{"error":"invalid_grant"}`, true, false},
		{"token 500", http.StatusInternalServerError, http.StatusOK, `oops`, false, true},
	}
	for _, tc := range cases {
		f.set(tc.token, tc.models, tc.tokenBody)
		res := probe(key)
		// An AuthFailed result may carry the refusal as detail in Err.
		if res.AuthFailed != tc.wantAuth || (!tc.wantAuth && (res.Err != nil) != tc.wantError) || res.Healthy {
			t.Errorf("%s: %+v, want AuthFailed=%v Err=%v", tc.name, res, tc.wantAuth, tc.wantError)
		}
	}

	// A credential that is not a service-account key can never authenticate.
	if res := probe("sk-not-a-key"); !res.AuthFailed || res.Err == nil {
		t.Errorf("malformed key: %+v, want AuthFailed with the parse error", res)
	}
}

// The location comes from a regional endpoint's host; any other host,
// including the global endpoint, uses the global location.
func TestVertexLocation(t *testing.T) {
	cases := map[string]string{
		"https://us-central1-aiplatform.googleapis.com":       "us-central1",
		"https://europe-west4-aiplatform.googleapis.com/":     "europe-west4",
		"https://aiplatform.googleapis.com":                   "global",
		"https://vertex-proxy.internal.example.com/some/path": "global",
		"https://[::1": "global",
	}
	for endpoint, want := range cases {
		if got := vertexLocation(endpoint); got != want {
			t.Errorf("vertexLocation(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
