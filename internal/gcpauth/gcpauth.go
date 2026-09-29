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

// Package gcpauth mints Google Cloud OAuth2 access tokens from a
// service-account JSON key, the credential a google-vertex ModelProvider's
// Secret holds. It is shared so the controller's liveness probe and the
// gateway use one implementation; it depends on neither.
package gcpauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

const (
	// CloudPlatformScope is the scope minted tokens carry.
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// DefaultTokenURL is Google's token endpoint, used when the key names no
	// token_uri.
	DefaultTokenURL = "https://oauth2.googleapis.com/token"
)

var (
	// ErrInvalidKey reports a credential that is not a usable
	// service-account JSON key.
	ErrInvalidKey = errors.New("invalid service-account key")
	// ErrRejected reports a token endpoint that refused the key: a 401 or
	// 403, or a 400 carrying invalid_grant or invalid_client.
	ErrRejected = errors.New("token endpoint rejected the service-account key")
)

// ServiceAccountKey is the parsed subset of a service-account JSON key.
type ServiceAccountKey struct {
	ProjectID    string
	ClientEmail  string
	PrivateKeyID string
	// TokenURL is the key's token_uri, or DefaultTokenURL.
	TokenURL   string
	privateKey []byte
}

// ParseServiceAccountKey parses a service-account JSON key. Every failure
// wraps ErrInvalidKey. The private key must be a PEM RSA key (PKCS#8 or
// PKCS#1) and token_uri, when set, an https URL.
func ParseServiceAccountKey(raw []byte) (*ServiceAccountKey, error) {
	var f struct {
		Type         string `json:"type"`
		ProjectID    string `json:"project_id"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		ClientEmail  string `json:"client_email"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: not JSON", ErrInvalidKey)
	}
	if f.Type != "service_account" {
		return nil, fmt.Errorf("%w: type is %q, want service_account", ErrInvalidKey, f.Type)
	}
	for name, v := range map[string]string{
		"project_id": f.ProjectID, "client_email": f.ClientEmail, "private_key": f.PrivateKey,
	} {
		if v == "" {
			return nil, fmt.Errorf("%w: %s is missing", ErrInvalidKey, name)
		}
	}
	if err := checkRSAKey([]byte(f.PrivateKey)); err != nil {
		return nil, fmt.Errorf("%w: private_key: %v", ErrInvalidKey, err)
	}
	tokenURL := DefaultTokenURL
	if f.TokenURI != "" {
		u, err := url.Parse(f.TokenURI)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("%w: token_uri must be an https URL", ErrInvalidKey)
		}
		tokenURL = f.TokenURI
	}
	return &ServiceAccountKey{
		ProjectID: f.ProjectID, ClientEmail: f.ClientEmail, PrivateKeyID: f.PrivateKeyID,
		TokenURL: tokenURL, privateKey: []byte(f.PrivateKey),
	}, nil
}

func checkRSAKey(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return errors.New("not PEM")
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return nil
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return nil
	}
	return errors.New("not an RSA private key")
}

// AccessToken exchanges a JWT bearer assertion signed with the key for an
// access token at the key's token endpoint, over hc (http.DefaultClient
// when nil). A refusal of the key wraps ErrRejected; a network error or any
// other non-2xx is returned as is.
func (k *ServiceAccountKey) AccessToken(ctx context.Context, hc *http.Client) (string, error) {
	if hc != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	}
	cfg := &jwt.Config{
		Email:        k.ClientEmail,
		PrivateKey:   k.privateKey,
		PrivateKeyID: k.PrivateKeyID,
		Scopes:       []string{CloudPlatformScope},
		TokenURL:     k.TokenURL,
	}
	tok, err := cfg.TokenSource(ctx).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && rejected(re.Response.StatusCode, re.Body) {
			return "", fmt.Errorf("%w: HTTP %d", ErrRejected, re.Response.StatusCode)
		}
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("token endpoint returned no access_token")
	}
	return tok.AccessToken, nil
}

// rejected classifies a token endpoint error: 401 and 403 always refuse the
// key; a 400 does when its OAuth2 error code says the grant or client is
// invalid (a revoked key, a bad signature, an unknown account).
func rejected(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return e.Error == "invalid_grant" || e.Error == "invalid_client"
	}
	return false
}
