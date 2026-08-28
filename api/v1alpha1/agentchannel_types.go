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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentChannelSpec binds an inbound channel (a generic webhook, or since
// v0.7.0 a Discord or WhatsApp platform adapter) to an Agent. See
// docs/src/resources/agentchannel.md. Rule 39: the type selects exactly one
// configuration block, and that block must be the one set.
// +kubebuilder:validation:XValidation:rule="((has(self.type) ? self.type : 'webhook') == 'webhook') == has(self.webhook) && ((has(self.type) ? self.type : 'webhook') == 'discord') == has(self.discord) && ((has(self.type) ? self.type : 'webhook') == 'whatsapp') == has(self.whatsapp)",message="exactly the block matching spec.type must be set (webhook, discord, or whatsapp)"
type AgentChannelSpec struct {
	// AgentRef names the target Agent. It must be an Agent, never an AgentTask,
	// in the same namespace.
	// +kubebuilder:validation:Required
	AgentRef LocalObjectReference `json:"agentRef"`
	// Type selects the channel platform: the generic webhook, or one of the
	// platform adapters (since v0.7.0).
	// +kubebuilder:validation:Enum=webhook;discord;whatsapp
	// +kubebuilder:default=webhook
	// +optional
	Type string `json:"type,omitempty"`
	// Webhook configures the webhook channel. Required when type is webhook.
	// +optional
	Webhook *AgentChannelWebhook `json:"webhook,omitempty"`
	// Discord configures a Discord Interactions endpoint channel. Required
	// when type is discord.
	// +optional
	Discord *AgentChannelDiscord `json:"discord,omitempty"`
	// WhatsApp configures a WhatsApp Cloud API webhook channel. Required when
	// type is whatsapp.
	// +optional
	WhatsApp *AgentChannelWhatsApp `json:"whatsapp,omitempty"`
	// Session configures deterministic session identity.
	// +optional
	Session AgentChannelSession `json:"session,omitempty"`
}

// Path returns the inbound path of whichever block the type selects, or the
// empty string when the spec is malformed. Every consumer that keys on the
// channel's path (routing, health, conflicts) goes through this.
func (s *AgentChannelSpec) Path() string {
	switch {
	case s.Discord != nil && s.Type == ChannelTypeDiscord:
		return s.Discord.Path
	case s.WhatsApp != nil && s.Type == ChannelTypeWhatsApp:
		return s.WhatsApp.Path
	case s.Webhook != nil:
		return s.Webhook.Path
	}
	return ""
}

// AgentChannelDiscord configures the Discord Interactions endpoint adapter
// (docs/src/resources/agentchannel.md, Platform types). Discord POSTs signed
// interactions to path; the gateway answers through the interaction's
// follow-up webhook.
type AgentChannelDiscord struct {
	// Path is the inbound path Discord posts interactions to. Same rules as
	// webhook.path: /channels/{namespace}/ prefix (rule 15, reconcile time) and
	// never the reserved /v1/ prefix (rule 16).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/v1/')",message="discord.path must not use the reserved /v1/ prefix"
	Path string `json:"path"`
	// CredentialsRef names a Secret in the channel's namespace carrying
	// publicKey (required; the application's Ed25519 public key, hex) and
	// botToken (optional; lets replies outlive the 15-minute interaction
	// token). Rule 40.
	// +kubebuilder:validation:Required
	CredentialsRef LocalObjectReference `json:"credentialsRef"`
	// GuildID, when set, restricts the channel to interactions from one guild;
	// interactions from elsewhere (other guilds, DMs) get an ephemeral refusal.
	// +kubebuilder:validation:Pattern=`^[0-9]{17,20}$`
	// +optional
	GuildID *string `json:"guildId,omitempty"`
	// AllowedChannelIDs, when set, restricts the channel to interactions from
	// these Discord channels.
	// +kubebuilder:validation:items:Pattern=`^[0-9]{17,20}$`
	// +listType=set
	// +optional
	AllowedChannelIDs []string `json:"allowedChannelIds,omitempty"`
	// ContentOption names the slash-command option whose string value becomes
	// the envelope's content.
	// +kubebuilder:default=message
	// +kubebuilder:validation:Pattern=`^[-_a-z0-9]{1,32}$`
	// +optional
	ContentOption string `json:"contentOption,omitempty"`
}

// AgentChannelWhatsApp configures the WhatsApp Cloud API webhook adapter
// (docs/src/resources/agentchannel.md, Platform types). Meta verifies path
// with a GET and POSTs signed events to it; the gateway replies through the
// Graph API as the configured business number.
type AgentChannelWhatsApp struct {
	// Path is the inbound path Meta posts events to. Same rules as
	// webhook.path (rules 15 and 16).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/v1/')",message="whatsapp.path must not use the reserved /v1/ prefix"
	Path string `json:"path"`
	// CredentialsRef names a Secret in the channel's namespace carrying
	// verifyToken, appSecret, and accessToken, all required. Rule 40.
	// +kubebuilder:validation:Required
	CredentialsRef LocalObjectReference `json:"credentialsRef"`
	// PhoneNumberID is the business phone number this channel answers as.
	// Events for other numbers under the same app are acknowledged and dropped.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	PhoneNumberID string `json:"phoneNumberId"`
}

// AgentChannelWebhook configures inbound and outbound webhook behavior.
// callbackAuth is required whenever callbackUrl is set (rule 25).
// +kubebuilder:validation:XValidation:rule="!has(self.callbackUrl) || has(self.callbackAuth)",message="callbackAuth is required when callbackUrl is set"
type AgentChannelWebhook struct {
	// Path is the inbound webhook path. It must begin with /channels/{namespace}/
	// (the namespace prefix is checked at reconcile time) and must not use the
	// reserved /v1/ prefix (rule 16).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/v1/')",message="webhook.path must not use the reserved /v1/ prefix"
	Path string `json:"path"`
	// Auth authenticates inbound webhook calls.
	// +kubebuilder:validation:Required
	Auth ChannelAuth `json:"auth"`
	// UserID extracts the caller identity from the request.
	// +optional
	UserID ChannelExtractor `json:"userId,omitempty"`
	// Content extracts the message content from the request.
	// +optional
	Content ChannelExtractor `json:"content,omitempty"`
	// ResponseMode selects synchronous inline delivery or asynchronous
	// (202 + callback/polling) delivery.
	// +kubebuilder:validation:Enum=sync;async
	// +kubebuilder:default=sync
	// +optional
	ResponseMode string `json:"responseMode,omitempty"`
	// CallbackURL, when set, receives async responses. Must be HTTPS and must not
	// point into internal address space (rule 22, checked at reconcile time).
	// +optional
	CallbackURL *string `json:"callbackUrl,omitempty"`
	// CallbackAuth signs outbound callbacks. Required when callbackUrl is set.
	// +optional
	CallbackAuth *ChannelAuth `json:"callbackAuth,omitempty"`
	// MaxPendingAsyncResponses caps concurrent in-flight async responses.
	// +kubebuilder:default=100
	// +optional
	MaxPendingAsyncResponses int32 `json:"maxPendingAsyncResponses,omitempty"`
}

// ChannelAuth authenticates a webhook direction. bearer requires secretRef; hmac
// requires the hmac block.
// +kubebuilder:validation:XValidation:rule="(self.type == 'bearer' && has(self.secretRef)) || (self.type == 'hmac' && has(self.hmac))",message="bearer auth requires secretRef; hmac auth requires the hmac block"
type ChannelAuth struct {
	// Type selects the auth scheme.
	// +kubebuilder:validation:Enum=bearer;hmac
	// +kubebuilder:validation:Required
	Type string `json:"type"`
	// SecretRef holds the bearer token. Required for bearer.
	// +optional
	SecretRef *SecretKeyReference `json:"secretRef,omitempty"`
	// HMAC configures signature verification. Required for hmac.
	// +optional
	HMAC *ChannelHMAC `json:"hmac,omitempty"`
}

// ChannelHMAC configures HMAC signature verification.
type ChannelHMAC struct {
	// Header carrying the signature.
	// +kubebuilder:validation:Required
	Header string `json:"header"`
	// Algorithm used to compute the signature.
	// +kubebuilder:validation:Enum=sha256;sha1
	// +kubebuilder:default=sha256
	// +optional
	Algorithm string `json:"algorithm,omitempty"`
	// SecretRef holds the signing key.
	// +kubebuilder:validation:Required
	SecretRef SecretKeyReference `json:"secretRef"`
	// SignaturePrefix is stripped from the header value before comparison.
	// +optional
	SignaturePrefix *string `json:"signaturePrefix,omitempty"`
	// Encoding of the signature in the header.
	// +kubebuilder:validation:Enum=hex;base64
	// +kubebuilder:default=hex
	// +optional
	Encoding string `json:"encoding,omitempty"`
}

// ChannelExtractor pulls a field from the request. fromHeader and fromBody are
// mutually exclusive; fromBody is a dotted path into a JSON body.
// +kubebuilder:validation:XValidation:rule="!(has(self.fromHeader) && has(self.fromBody))",message="fromHeader and fromBody are mutually exclusive"
type ChannelExtractor struct {
	// FromHeader names a request header.
	// +optional
	FromHeader *string `json:"fromHeader,omitempty"`
	// FromBody is a dotted path into the JSON request body (for example
	// "user.id").
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$`
	// +optional
	FromBody *string `json:"fromBody,omitempty"`
	// Fallback value used when extraction yields nothing.
	// +optional
	Fallback *string `json:"fallback,omitempty"`
}

// AgentChannelSession configures deterministic sessions.
type AgentChannelSession struct {
	// Enabled derives a deterministic sessionId per (channel, user).
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AgentChannelStatus is the observed state of an AgentChannel.
type AgentChannelStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the current lifecycle phase. Unset until first reconcile.
	// +optional
	Phase AgentChannelPhase `json:"phase,omitempty"`
	// Conditions report Ready and the tri-state PlatformConnected.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:deprecatedversion:warning="kaalm.io/v1alpha1 AgentChannel is deprecated; use kaalm.io/v1beta1"
// +kubebuilder:resource:shortName=ach
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Connected",type=string,JSONPath=`.status.conditions[?(@.type=="PlatformConnected")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentChannel is the Schema for the agentchannels API.
type AgentChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentChannelSpec   `json:"spec,omitempty"`
	Status AgentChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentChannelList contains a list of AgentChannel.
type AgentChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentChannel{}, &AgentChannelList{})
}
