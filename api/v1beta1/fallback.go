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

package v1beta1

// ModelProvider types (spec.type).
const (
	ProviderTypeAnthropic        = "anthropic"
	ProviderTypeOpenAI           = "openai"
	ProviderTypeOpenAICompatible = "openai-compatible"
	ProviderTypeGoogleVertex     = "google-vertex"
)

// ProviderFormat is the wire format a provider type speaks: openai and
// openai-compatible share one; anthropic and google-vertex have their own.
func ProviderFormat(providerType string) string {
	if providerType == ProviderTypeOpenAICompatible {
		return ProviderTypeOpenAI
	}
	return providerType
}

// FallbackFormatCompatible is rule 12 (docs/src/resources/validation-and-defaulting.md):
// an edge may join providers of the same type, or since v0.7.0 anthropic
// and openai / openai-compatible in either direction, the pair the gateway
// translates between. google-vertex stays same-type.
func FallbackFormatCompatible(parent, child string) bool {
	if parent == child {
		return true
	}
	return translatableFormat(parent) && translatableFormat(child)
}

// FallbackCrossesFormat reports whether an edge needs translation.
func FallbackCrossesFormat(parent, child string) bool {
	return ProviderFormat(parent) != ProviderFormat(child)
}

func translatableFormat(providerType string) bool {
	switch providerType {
	case ProviderTypeAnthropic, ProviderTypeOpenAI, ProviderTypeOpenAICompatible:
		return true
	}
	return false
}
