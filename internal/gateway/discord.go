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
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The Discord adapter: the Interactions endpoint
// (docs/src/gateways/api/channel-discord.md). Discord POSTs signed
// interactions to the channel's path; the adapter acknowledges with a
// deferred response inside Discord's 3-second budget and answers later
// through the interaction's follow-up webhook.

const (
	// Interaction types Discord sends.
	discordInteractionPing         = 1
	discordInteractionCommand      = 2
	discordInteractionComponent    = 3
	discordInteractionAutocomplete = 4
	discordInteractionModal        = 5

	// Interaction response types the adapter answers with.
	discordResponsePong            = 1
	discordResponseMessage         = 4
	discordResponseDeferredMessage = 5
	discordResponseDeferredUpdate  = 6
	discordResponseAutocomplete    = 8

	discordFlagEphemeral    = 64
	discordOptionAttachment = 11

	// discordChunkLimit is Discord's message length, in characters.
	discordChunkLimit = 2000
	// discordTokenWindow is how long an interaction token stays valid.
	discordTokenWindow = 15 * time.Minute
	// discordTimestampSkew bounds replay on the signed timestamp, the same
	// bound the polling endpoint uses.
	discordTimestampSkew = 300 * time.Second

	discordRefusalText = "This bot is not available here."
	discordModalText   = "Modals are not supported."
	discordEmptyReply  = "(empty reply)"

	// DefaultDiscordAPIBaseURL is where replies go unless
	// gateway.platforms.discord.apiBaseUrl says otherwise.
	DefaultDiscordAPIBaseURL = "https://discord.com/api/v10"

	// The credential Secret keys (rule 40).
	discordKeyPublicKey = "publicKey"
	discordKeyBotToken  = "botToken"
)

type discordAdapter struct{ s *Server }

func (d *discordAdapter) Type() string { return kaalmv1beta1.ChannelTypeDiscord }

// discordInteraction is the subset of the interaction object the adapter
// reads.
type discordInteraction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	GuildID       string `json:"guild_id"`
	ChannelID     string `json:"channel_id"`
	Member        *struct {
		User *discordUser `json:"user"`
	} `json:"member"`
	User *discordUser `json:"user"`
	Data *struct {
		Name     string          `json:"name"`
		Options  []discordOption `json:"options"`
		Resolved *struct {
			Attachments map[string]discordAttachment `json:"attachments"`
		} `json:"resolved"`
	} `json:"data"`
	Locale string `json:"locale"`
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type discordOption struct {
	Name  string `json:"name"`
	Type  int    `json:"type"`
	Value any    `json:"value"`
}

type discordAttachment struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// discordReply is the adapter-private reply context: what the follow-up
// webhook and the bot-token fallback need. It stays in memory.
type discordReply struct {
	applicationID string
	token         string
	channelID     string
	userID        string
	received      time.Time
}

// Handle answers every interaction inside the request, per the response
// table on the wire-contract page.
func (d *discordAdapter) Handle(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	channel *kaalmv1beta1.AgentChannel, body []byte,
) inboundResult {
	if r.Method != http.MethodPost {
		unauthorized(w, "unknown path")
		return inboundResult{}
	}
	pub, err := d.publicKey(ctx, channel)
	if err != nil {
		unauthorized(w, "auth failed or path not registered")
		return inboundResult{authFailed: "discord publicKey unusable: " + err.Error()}
	}
	if !verifyDiscordSignature(pub, r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"),
		body, time.Now()) {
		unauthorized(w, "auth failed or path not registered")
		return inboundResult{authFailed: "discord signature or timestamp rejected: 401 Unauthorized"}
	}
	var in discordInteraction
	if err := json.Unmarshal(body, &in); err != nil {
		badRequest(w, "interaction body is not valid JSON")
		return inboundResult{}
	}
	switch in.Type {
	case discordInteractionPing:
		writeDiscordResponse(w, map[string]any{"type": discordResponsePong})
	case discordInteractionCommand:
		if !d.inScope(channel, &in) {
			writeDiscordEphemeral(w, discordRefusalText)
			return inboundResult{rejected: 1}
		}
		msg := d.buildMessage(channel, &in)
		writeDiscordResponse(w, map[string]any{"type": discordResponseDeferredMessage})
		return inboundResult{messages: []platformMessage{msg}}
	case discordInteractionComponent:
		writeDiscordResponse(w, map[string]any{"type": discordResponseDeferredUpdate})
	case discordInteractionAutocomplete:
		writeDiscordResponse(w, map[string]any{
			"type": discordResponseAutocomplete, "data": map[string]any{"choices": []any{}}})
	case discordInteractionModal:
		writeDiscordEphemeral(w, discordModalText)
	default:
		badRequest(w, fmt.Sprintf("unknown interaction type %d", in.Type))
	}
	return inboundResult{}
}

// publicKey loads and decodes the channel's Ed25519 public key through the
// scoped Secret read.
func (d *discordAdapter) publicKey(ctx context.Context, channel *kaalmv1beta1.AgentChannel) (ed25519.PublicKey, error) {
	raw, err := d.s.Store.SecretValue(ctx, channel.Namespace, channel.Spec.Discord.CredentialsRef.Name, discordKeyPublicKey)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("publicKey is not hex: %w", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("publicKey is %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), nil
}

// verifyDiscordSignature checks Ed25519(publicKey, timestamp || body) and
// bounds the timestamp to discordTimestampSkew around now.
func verifyDiscordSignature(pub ed25519.PublicKey, sigHex, timestamp string, body []byte, now time.Time) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > discordTimestampSkew {
		return false
	}
	signed := make([]byte, 0, len(timestamp)+len(body))
	signed = append(signed, timestamp...)
	signed = append(signed, body...)
	return ed25519.Verify(pub, signed, sig)
}

// inScope applies guildId and allowedChannelIds.
func (d *discordAdapter) inScope(channel *kaalmv1beta1.AgentChannel, in *discordInteraction) bool {
	cfg := channel.Spec.Discord
	if cfg.GuildID != nil && in.GuildID != *cfg.GuildID {
		return false
	}
	if len(cfg.AllowedChannelIDs) > 0 {
		allowed := false
		for _, id := range cfg.AllowedChannelIDs {
			if id == in.ChannelID {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// buildMessage normalizes an in-scope command into the envelope (wire
// contract: Discord Channel, Normalization) and keeps the reply context.
func (d *discordAdapter) buildMessage(channel *kaalmv1beta1.AgentChannel, in *discordInteraction) platformMessage {
	cfg := channel.Spec.Discord
	user := in.User
	if in.Member != nil && in.Member.User != nil {
		user = in.Member.User
	}
	userID := ""
	if user != nil {
		userID = user.ID
	}
	contentOption := cfg.ContentOption
	if contentOption == "" {
		contentOption = "message"
	}
	command := ""
	options := map[string]any{}
	content := ""
	attachments := []json.RawMessage{}
	if in.Data != nil {
		command = in.Data.Name
		for _, opt := range in.Data.Options {
			options[opt.Name] = opt.Value
			if opt.Name == contentOption {
				if s, ok := opt.Value.(string); ok {
					content = s
				}
			}
			if opt.Type == discordOptionAttachment && in.Data.Resolved != nil {
				id, _ := opt.Value.(string)
				if att, ok := in.Data.Resolved.Attachments[id]; ok {
					raw, _ := json.Marshal(map[string]any{
						"type": "discord.attachment", "id": att.ID, "url": att.URL,
						"filename": att.Filename, "contentType": att.ContentType, "size": att.Size,
					})
					attachments = append(attachments, raw)
				}
			}
		}
	}
	env := MessageEnvelope{
		MessageID:   uuid.NewString(),
		ChannelType: kaalmv1beta1.ChannelTypeDiscord,
		ChannelID:   channel.Spec.Path(),
		UserID:      userID,
		Content:     content,
		Attachments: attachments,
		Metadata: map[string]any{
			"interactionId": in.ID, "applicationId": in.ApplicationID,
			"guildId": in.GuildID, "channelId": in.ChannelID,
			"command": command, "options": options, "locale": in.Locale,
		},
	}
	if channel.Spec.Session.Enabled {
		env.SessionID = SessionID(env.ChannelID, env.UserID)
	}
	return platformMessage{env: env, reply: discordReply{
		applicationID: in.ApplicationID, token: in.Token, channelID: in.ChannelID,
		userID: userID, received: time.Now(),
	}}
}

func writeDiscordResponse(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func writeDiscordEphemeral(w http.ResponseWriter, text string) {
	writeDiscordResponse(w, map[string]any{
		"type": discordResponseMessage,
		"data": map[string]any{"content": text, "flags": discordFlagEphemeral},
	})
}

// apiBaseURL is the operator-set base URL replies go to.
func (d *discordAdapter) apiBaseURL() string {
	if d.s.Config.DiscordAPIBaseURL != "" {
		return d.s.Config.DiscordAPIBaseURL
	}
	return DefaultDiscordAPIBaseURL
}

// SendReply edits the deferred message with the first chunk and posts the
// rest as follow-ups. Past the token window, or on a 404 from the follow-up
// webhook, it switches to channel messages when botToken is configured.
func (d *discordAdapter) SendReply(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, msg platformMessage, text string,
) string {
	rc, ok := msg.reply.(discordReply)
	if !ok {
		return callbackRejected
	}
	if text == "" {
		text = discordEmptyReply
	}
	chunks := splitChunks(text, discordChunkLimit)
	if time.Since(rc.received) > discordTokenWindow {
		return d.sendViaBot(ctx, channel, rc, chunks, "interaction token expired")
	}
	base := d.apiBaseURL()
	webhook := fmt.Sprintf("%s/webhooks/%s/%s", base, rc.applicationID, rc.token)
	for i, chunk := range chunks {
		method, url := http.MethodPost, webhook
		if i == 0 {
			method, url = http.MethodPatch, webhook+"/messages/@original"
		}
		res := d.s.sendPlatformRequest(ctx, func(ctx context.Context) (*http.Request, error) {
			return discordJSONRequest(ctx, method, url, "", map[string]any{"content": chunk})
		}, func(status int, _ []byte) replyBucket { return classifyReplyStatus(status) })
		if res.bucket == bucketDelivered {
			continue
		}
		if res.status == http.StatusNotFound && i == 0 {
			return d.sendViaBot(ctx, channel, rc, chunks, "follow-up webhook answered 404")
		}
		return d.s.replyRefused(channel, "discord", res)
	}
	return callbackDelivered
}

// sendViaBot posts the reply as channel messages with the bot token, one per
// chunk, each mentioning the user. Without a bot token the reply is refused.
func (d *discordAdapter) sendViaBot(
	ctx context.Context, channel *kaalmv1beta1.AgentChannel, rc discordReply, chunks []string, why string,
) string {
	token, err := d.s.Store.SecretValue(ctx, channel.Namespace, channel.Spec.Discord.CredentialsRef.Name, discordKeyBotToken)
	if err != nil || token == "" {
		return d.s.replyRefused(channel, "discord", replyResult{
			bucket: bucketTerminal, status: http.StatusNotFound,
			body: []byte(why + " and no botToken is configured")})
	}
	url := fmt.Sprintf("%s/channels/%s/messages", d.apiBaseURL(), rc.channelID)
	for _, chunk := range chunks {
		body := map[string]any{
			"content":          "<@" + rc.userID + "> " + chunk,
			"allowed_mentions": map[string]any{"users": []string{rc.userID}},
		}
		res := d.s.sendPlatformRequest(ctx, func(ctx context.Context) (*http.Request, error) {
			return discordJSONRequest(ctx, http.MethodPost, url, "Bot "+token, body)
		}, func(status int, _ []byte) replyBucket { return classifyReplyStatus(status) })
		if res.bucket != bucketDelivered {
			return d.s.replyRefused(channel, "discord", res)
		}
	}
	return callbackDelivered
}

func discordJSONRequest(ctx context.Context, method, url, authorization string, body map[string]any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req, nil
}
