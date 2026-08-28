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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The WhatsApp adapter: the Cloud API webhook
// (docs/src/gateways/api/channel-whatsapp.md). Meta verifies the channel's
// path with a GET and POSTs signed events to it; the adapter answers 200 at
// once and replies later through the Graph API as the channel's business
// number.

const (
	// The credential Secret keys (rule 40).
	whatsAppKeyVerifyToken = "verifyToken"
	whatsAppKeyAppSecret   = "appSecret"
	whatsAppKeyAccessToken = "accessToken"

	whatsAppSignatureHeader = "X-Hub-Signature-256"
	whatsAppSignaturePrefix = "sha256="
	// whatsAppChunkLimit is the Cloud API's text body length.
	whatsAppChunkLimit = 4096
	// whatsAppErrorRateLimit is Meta's rate-limit error code, the one 400
	// that is retried.
	whatsAppErrorRateLimit = 130429

	whatsAppEmptyReply = "(empty reply)"
	// whatsAppMetadataMessageKey carries the raw inbound message object.
	whatsAppMetadataMessageKey = "message"

	// DefaultWhatsAppAPIBaseURL is where replies go unless
	// gateway.platforms.whatsapp.apiBaseUrl says otherwise. It carries the
	// Graph API version segment.
	DefaultWhatsAppAPIBaseURL = "https://graph.facebook.com/v23.0"
)

type whatsAppAdapter struct{ s *Server }

func (w *whatsAppAdapter) Type() string { return kaalmv1beta1.ChannelTypeWhatsApp }

// whatsAppEvent is the subset of the webhook event the adapter reads.
type whatsAppEvent struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []json.RawMessage `json:"messages"`
				Statuses []json.RawMessage `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// whatsAppMessage is one inbound message, typed enough to pick the content.
type whatsAppMessage struct {
	From        string                 `json:"from"`
	ID          string                 `json:"id"`
	Timestamp   string                 `json:"timestamp"`
	Type        string                 `json:"type"`
	Text        *struct{ Body string } `json:"text"`
	Interactive *struct {
		ButtonReply *struct{ Title string } `json:"button_reply"`
		ListReply   *struct{ Title string } `json:"list_reply"`
	} `json:"interactive"`
	Image    *whatsAppMedia `json:"image"`
	Document *whatsAppMedia `json:"document"`
	Audio    *whatsAppMedia `json:"audio"`
	Video    *whatsAppMedia `json:"video"`
	Sticker  *whatsAppMedia `json:"sticker"`
}

type whatsAppMedia struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Caption  string `json:"caption"`
}

// whatsAppAttachmentRef is the envelope's attachment reference: the media
// id the agent could fetch through the Graph API, never the bytes.
type whatsAppAttachmentRef struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	MimeType string `json:"mimeType"`
	SHA256   string `json:"sha256"`
}

// whatsAppReply is the adapter-private reply context: who to answer as, and
// whom.
type whatsAppReply struct {
	phoneNumberID string
	to            string
}

// whatsAppSendBody is the Graph API text message.
type whatsAppSendBody struct {
	MessagingProduct string           `json:"messaging_product"`
	RecipientType    string           `json:"recipient_type"`
	To               string           `json:"to"`
	Type             string           `json:"type"`
	Text             whatsAppSendText `json:"text"`
}

type whatsAppSendText struct {
	PreviewURL bool   `json:"preview_url"`
	Body       string `json:"body"`
}

// whatsAppErrorBody is the Graph API error envelope, read for the code.
type whatsAppErrorBody struct {
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
}

// Handle answers the verification GET and every event POST inside the
// request, per the response tables on the wire-contract page.
func (w *whatsAppAdapter) Handle(
	ctx context.Context, rw http.ResponseWriter, r *http.Request,
	channel *kaalmv1beta1.AgentChannel, body []byte,
) inboundResult {
	switch r.Method {
	case http.MethodGet:
		return w.handleVerification(ctx, rw, r, channel)
	case http.MethodPost:
		return w.handleEvent(ctx, rw, r, channel, body)
	}
	unauthorized(rw, "unknown path")
	return inboundResult{}
}

// handleVerification is the handshake Meta performs when the operator saves
// the URL: echo hub.challenge when hub.verify_token matches.
func (w *whatsAppAdapter) handleVerification(
	ctx context.Context, rw http.ResponseWriter, r *http.Request, channel *kaalmv1beta1.AgentChannel,
) inboundResult {
	q := r.URL.Query()
	token, err := w.s.Store.SecretValue(ctx, channel.Namespace, channel.Spec.WhatsApp.CredentialsRef.Name,
		whatsAppKeyVerifyToken)
	if err != nil {
		forbidden(rw, errUnauthorized, "verification failed")
		return inboundResult{authFailed: "whatsapp verifyToken unavailable: " + err.Error()}
	}
	if q.Get("hub.mode") != "subscribe" ||
		subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(token)) != 1 {
		forbidden(rw, errUnauthorized, "verification failed")
		return inboundResult{authFailed: "whatsapp verification token rejected: 403 Forbidden"}
	}
	rw.Header().Set("Content-Type", "text/plain")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(q.Get("hub.challenge")))
	return inboundResult{}
}

// handleEvent verifies the signature, acknowledges, and emits one message
// per inbound message for this channel's number.
func (w *whatsAppAdapter) handleEvent(
	ctx context.Context, rw http.ResponseWriter, r *http.Request,
	channel *kaalmv1beta1.AgentChannel, body []byte,
) inboundResult {
	secret, err := w.s.Store.SecretValue(ctx, channel.Namespace, channel.Spec.WhatsApp.CredentialsRef.Name,
		whatsAppKeyAppSecret)
	if err != nil {
		unauthorized(rw, "auth failed or path not registered")
		return inboundResult{authFailed: "whatsapp appSecret unavailable: " + err.Error()}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	prefix := whatsAppSignaturePrefix
	cfg := &kaalmv1beta1.ChannelHMAC{Header: whatsAppSignatureHeader, Algorithm: "sha256",
		SignaturePrefix: &prefix, Encoding: "hex"}
	if !verifyHMACHeader(r.Header.Get(whatsAppSignatureHeader), cfg, mac.Sum(nil)) {
		unauthorized(rw, "auth failed or path not registered")
		return inboundResult{authFailed: "whatsapp signature rejected: 401 Unauthorized"}
	}
	var event whatsAppEvent
	if err := json.Unmarshal(body, &event); err != nil {
		badRequest(rw, "event body is not valid JSON")
		return inboundResult{}
	}
	// The 200 goes out before any delivery is attempted, so Meta's
	// retry-on-failure never duplicates a message the gateway accepted.
	rw.WriteHeader(http.StatusOK)

	var res inboundResult
	mine := channel.Spec.WhatsApp.PhoneNumberID
	for _, entry := range event.Entry {
		for _, change := range entry.Changes {
			v := change.Value
			res.rejected += len(v.Statuses)
			if v.Metadata.PhoneNumberID != mine {
				res.rejected += len(v.Messages)
				continue
			}
			names := map[string]string{}
			for _, c := range v.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for _, raw := range v.Messages {
				var m whatsAppMessage
				if err := json.Unmarshal(raw, &m); err != nil || m.From == "" {
					res.rejected++
					continue
				}
				res.messages = append(res.messages, w.buildMessage(channel, &m, raw,
					v.Metadata.DisplayPhoneNumber, names[m.From]))
			}
		}
	}
	return res
}

// buildMessage normalizes one inbound message into the envelope (wire
// contract: WhatsApp Channel, Normalization) and keeps the reply context.
func (w *whatsAppAdapter) buildMessage(
	channel *kaalmv1beta1.AgentChannel, m *whatsAppMessage, raw json.RawMessage, displayNumber, profileName string,
) platformMessage {
	content := ""
	attachments := []json.RawMessage{}
	var media *whatsAppMedia
	switch m.Type {
	case "text":
		if m.Text != nil {
			content = m.Text.Body
		}
	case "interactive":
		if m.Interactive != nil {
			switch {
			case m.Interactive.ButtonReply != nil:
				content = m.Interactive.ButtonReply.Title
			case m.Interactive.ListReply != nil:
				content = m.Interactive.ListReply.Title
			}
		}
	case "image":
		media = m.Image
	case "document":
		media = m.Document
	case "audio":
		media = m.Audio
	case "video":
		media = m.Video
	case "sticker":
		media = m.Sticker
	}
	if media != nil {
		content = media.Caption
		ref, _ := json.Marshal(whatsAppAttachmentRef{
			Type: "whatsapp." + m.Type, ID: media.ID, MimeType: media.MimeType, SHA256: media.SHA256})
		attachments = append(attachments, ref)
	}
	var rawMessage any
	_ = json.Unmarshal(raw, &rawMessage)
	env := MessageEnvelope{
		MessageID:   uuid.NewString(),
		ChannelType: kaalmv1beta1.ChannelTypeWhatsApp,
		ChannelID:   channel.Spec.Path(),
		UserID:      m.From,
		Content:     content,
		Attachments: attachments,
		Metadata: map[string]any{
			"messageId": m.ID, "timestamp": m.Timestamp,
			"phoneNumberId": channel.Spec.WhatsApp.PhoneNumberID, "displayPhoneNumber": displayNumber,
			"profileName": profileName, "messageType": m.Type, whatsAppMetadataMessageKey: rawMessage,
		},
	}
	if channel.Spec.Session.Enabled {
		env.SessionID = SessionID(env.ChannelID, env.UserID)
	}
	return platformMessage{env: env, reply: whatsAppReply{
		phoneNumberID: channel.Spec.WhatsApp.PhoneNumberID, to: m.From,
	}}
}

// apiBaseURL is the operator-set base URL replies go to.
func (w *whatsAppAdapter) apiBaseURL() string {
	if w.s.Config.WhatsAppAPIBaseURL != "" {
		return w.s.Config.WhatsAppAPIBaseURL
	}
	return DefaultWhatsAppAPIBaseURL
}

// classifyWhatsAppReply is the shared table plus Meta's one exception: the
// rate-limit code arrives inside a 400 and is retried; every other 400
// (131047, a reply outside the 24-hour window, among them) is terminal.
func classifyWhatsAppReply(status int, body []byte) replyBucket {
	if status == http.StatusBadRequest {
		var e whatsAppErrorBody
		if json.Unmarshal(body, &e) == nil && e.Error.Code == whatsAppErrorRateLimit {
			return bucketRetried
		}
	}
	return classifyReplyStatus(status)
}

// SendReply posts one text message per chunk, in order, as the channel's
// business number.
func (w *whatsAppAdapter) SendReply(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, msg platformMessage, text string,
) string {
	rc, ok := msg.reply.(whatsAppReply)
	if !ok {
		return callbackRejected
	}
	token, err := w.s.Store.SecretValue(ctx, channel.Namespace, channel.Spec.WhatsApp.CredentialsRef.Name,
		whatsAppKeyAccessToken)
	if err != nil {
		return w.s.replyRefused(channel, "whatsapp", replyResult{
			bucket: bucketTerminal, status: http.StatusUnauthorized, body: []byte("accessToken unavailable: " + err.Error())})
	}
	if text == "" {
		text = whatsAppEmptyReply
	}
	url := fmt.Sprintf("%s/%s/messages", w.apiBaseURL(), rc.phoneNumberID)
	for _, chunk := range splitChunks(text, whatsAppChunkLimit) {
		payload := whatsAppSendBody{
			MessagingProduct: "whatsapp", RecipientType: "individual", To: rc.to, Type: "text",
			Text: whatsAppSendText{PreviewURL: false, Body: chunk},
		}
		res := w.s.sendPlatformRequest(ctx, func(ctx context.Context) (*http.Request, error) {
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			return req, nil
		}, classifyWhatsAppReply)
		if res.bucket != bucketDelivered {
			return w.s.replyRefused(channel, "whatsapp", res)
		}
	}
	return callbackDelivered
}
