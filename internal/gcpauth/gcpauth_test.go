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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

type assertionClaims struct {
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
}

// verifyRS256 checks a compact JWS signed RS256 by pub and returns its
// claims.
func verifyRS256(token string, pub *rsa.PublicKey) (assertionClaims, error) {
	var claims assertionClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("not a compact JWS")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return claims, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	return claims, json.Unmarshal(payload, &claims)
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
		claims, err := verifyRS256(r.Form.Get("assertion"), &pk.PublicKey)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.Scope != CloudPlatformScope || claims.Iss != "sa@proj.iam.gserviceaccount.com" {
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
