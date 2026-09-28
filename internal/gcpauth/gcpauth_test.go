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

package gcpauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2/jws"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return pk, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func keyJSON(t *testing.T, fields map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseServiceAccountKey(t *testing.T) {
	_, pemKey := testKeyPEM(t)
	valid := map[string]string{
		"type": "service_account", "project_id": "proj", "private_key_id": "kid",
		"private_key": pemKey, "client_email": "sa@proj.iam.gserviceaccount.com",
	}
	k, err := ParseServiceAccountKey(keyJSON(t, valid))
	if err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if k.ProjectID != "proj" || k.ClientEmail != valid["client_email"] || k.TokenURL != DefaultTokenURL {
		t.Errorf("parsed %+v", k)
	}

	with := func(key, value string) []byte {
		m := map[string]string{}
		for k, v := range valid {
			m[k] = v
		}
		if value == "" {
			delete(m, key)
		} else {
			m[key] = value
		}
		return keyJSON(t, m)
	}
	bad := map[string][]byte{
		"not JSON":              []byte("sk-not-a-key"),
		"wrong type":            with("type", "authorized_user"),
		"no client_email":       with("client_email", ""),
		"no project_id":         with("project_id", ""),
		"no private_key":        with("private_key", ""),
		"private_key not PEM":   with("private_key", "garbage"),
		"plain-HTTP token_uri":  with("token_uri", "http://oauth2.example.com/token"),
		"unparseable token_uri": with("token_uri", "https://[::1"),
	}
	for name, raw := range bad {
		if _, err := ParseServiceAccountKey(raw); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
}

// AccessToken signs a JWT bearer assertion with the key and exchanges it at
// token_uri; the token endpoint sees an assertion the key's public half
// verifies.
func TestAccessToken(t *testing.T) {
	pk, pemKey := testKeyPEM(t)
	status := http.StatusOK
	body := `{"access_token":"ya29.test","token_type":"Bearer","expires_in":3600}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assertion := r.Form.Get("assertion")
		if err := jws.Verify(assertion, &pk.PublicKey); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claims, err := jws.Decode(assertion)
		if err != nil || claims.Scope != CloudPlatformScope || claims.Iss != "sa@proj.iam.gserviceaccount.com" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	k, err := ParseServiceAccountKey(keyJSON(t, map[string]string{
		"type": "service_account", "project_id": "proj", "private_key": pemKey,
		"client_email": "sa@proj.iam.gserviceaccount.com", "token_uri": srv.URL + "/token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tok, err := k.AccessToken(ctx, srv.Client())
	if err != nil || tok != "ya29.test" {
		t.Fatalf("AccessToken = %q, %v; want ya29.test", tok, err)
	}

	for _, rejected := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`},
		{http.StatusUnauthorized, `{"error":"invalid_client"}`},
		{http.StatusForbidden, `{"error":"access_denied"}`},
	} {
		status, body = rejected.status, rejected.body
		if _, err := k.AccessToken(ctx, srv.Client()); !errors.Is(err, ErrRejected) {
			t.Errorf("token endpoint %d %s: err = %v, want ErrRejected", status, body, err)
		}
	}

	for _, transient := range []struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `oops`},
		{http.StatusBadRequest, `{"error":"invalid_scope"}`},
	} {
		status, body = transient.status, transient.body
		_, err := k.AccessToken(ctx, srv.Client())
		if err == nil || errors.Is(err, ErrRejected) {
			t.Errorf("token endpoint %d %s: err = %v, want a non-rejection error", status, body, err)
		}
	}

	status, body = http.StatusOK, `{"token_type":"Bearer"}`
	if _, err := k.AccessToken(ctx, srv.Client()); err == nil {
		t.Error("a response with no access_token must be an error")
	}
}
