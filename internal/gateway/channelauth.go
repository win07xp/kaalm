/*
Copyright 2026.

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
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // sha1 is an AgentChannel HMAC option for third-party webhook compatibility
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"time"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

const (
	authTypeBearer = "bearer"
	authTypeHMAC   = "hmac"

	// timestampHeader carries the unix-seconds timestamp on HMAC poll
	// requests and outbound callbacks.
	timestampHeader = "X-Kaalm-Timestamp"
	// maxTimestampSkew bounds HMAC timestamp drift.
	maxTimestampSkew = 300 * time.Second
)

// channelSecret resolves an auth block's secret material via the Store.
func (s *Server) channelSecret(ctx context.Context, namespace string, auth *kaalmv1alpha1.ChannelAuth) (string, error) {
	switch auth.Type {
	case authTypeBearer:
		if auth.SecretRef == nil {
			return "", fmt.Errorf("bearer auth without secretRef")
		}
		return s.Store.SecretValue(ctx, namespace, auth.SecretRef.Name, auth.SecretRef.Key)
	case authTypeHMAC:
		if auth.HMAC == nil {
			return "", fmt.Errorf("hmac auth without hmac block")
		}
		return s.Store.SecretValue(ctx, namespace, auth.HMAC.SecretRef.Name, auth.HMAC.SecretRef.Key)
	}
	return "", fmt.Errorf("unknown auth type %q", auth.Type)
}

func hmacHasher(algorithm string) func() hash.Hash {
	if algorithm == "sha1" {
		return sha1.New
	}
	return sha256.New
}

// authenticateWebhook verifies an inbound webhook request against the
// channel's spec.webhook.auth. The HMAC input is the raw body alone (no
// timestamp) for third-party sender compatibility; see
// docs/src/gateways/api/channel-webhook.md.
func (s *Server) authenticateWebhook(
	ctx context.Context, channel *kaalmv1alpha1.AgentChannel, r *http.Request, body []byte,
) bool {
	auth := channel.Spec.Webhook.Auth
	secret, err := s.channelSecret(ctx, channel.Namespace, &auth)
	if err != nil {
		return false
	}
	switch auth.Type {
	case authTypeBearer:
		token, ok := bearerToken(r.Header.Get("Authorization"))
		return ok && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
	case authTypeHMAC:
		mac := hmac.New(hmacHasher(auth.HMAC.Algorithm), []byte(secret))
		mac.Write(body)
		return verifyHMACHeader(r.Header.Get(auth.HMAC.Header), auth.HMAC, mac.Sum(nil))
	}
	return false
}

// verifyHMACHeader strips the configured prefix, decodes per the configured
// encoding, and constant-time-compares against the expected digest.
func verifyHMACHeader(headerValue string, cfg *kaalmv1alpha1.ChannelHMAC, expected []byte) bool {
	if headerValue == "" {
		return false
	}
	if cfg.SignaturePrefix != nil {
		var ok bool
		headerValue, ok = strings.CutPrefix(headerValue, *cfg.SignaturePrefix)
		if !ok {
			return false
		}
	}
	var supplied []byte
	var err error
	if cfg.Encoding == "base64" {
		supplied, err = base64.StdEncoding.DecodeString(headerValue)
	} else {
		supplied, err = hex.DecodeString(strings.ToLower(headerValue))
	}
	if err != nil {
		return false
	}
	return hmac.Equal(supplied, expected)
}

// authenticatePoll verifies a polling GET against the channel's inbound auth.
// Poll requests have no body: bearer presents the same token; HMAC signs
// "{requestId}\n{timestamp}" with bare lowercase hex and a 300s skew bound.
func (s *Server) authenticatePoll(
	ctx context.Context, channel *kaalmv1alpha1.AgentChannel, r *http.Request, requestID string,
) bool {
	auth := channel.Spec.Webhook.Auth
	secret, err := s.channelSecret(ctx, channel.Namespace, &auth)
	if err != nil {
		return false
	}
	switch auth.Type {
	case authTypeBearer:
		token, ok := bearerToken(r.Header.Get("Authorization"))
		return ok && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
	case authTypeHMAC:
		ts := r.Header.Get(timestampHeader)
		unix, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return false
		}
		if skew := time.Since(time.Unix(unix, 0)); skew > maxTimestampSkew || skew < -maxTimestampSkew {
			return false
		}
		mac := hmac.New(hmacHasher(auth.HMAC.Algorithm), []byte(secret))
		_, _ = fmt.Fprintf(mac, "%s\n%s", requestID, ts)
		supplied, err := hex.DecodeString(strings.ToLower(r.Header.Get(auth.HMAC.Header)))
		if err != nil {
			return false
		}
		return hmac.Equal(supplied, mac.Sum(nil))
	}
	return false
}

// signCallback signs an outbound callback POST per the channel's
// callbackAuth, with a fresh timestamp per attempt. The HMAC canonical string
// is "{requestId}\n{timestamp}\n{sha256(body)}".
func signCallback(
	req *http.Request, auth *kaalmv1alpha1.ChannelAuth, secret, requestID string, body []byte, now time.Time,
) {
	switch auth.Type {
	case authTypeBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
	case authTypeHMAC:
		ts := strconv.FormatInt(now.Unix(), 10)
		bodyHash := sha256.Sum256(body)
		mac := hmac.New(hmacHasher(auth.HMAC.Algorithm), []byte(secret))
		_, _ = fmt.Fprintf(mac, "%s\n%s\n%s", requestID, ts, hex.EncodeToString(bodyHash[:]))
		req.Header.Set(auth.HMAC.Header, hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set(timestampHeader, ts)
	}
}
