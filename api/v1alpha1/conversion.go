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
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// v1alpha1 is a spoke: since v0.6.0 it is served, deprecated, and converted
// to and from the v1beta1 hub by the controller's conversion webhook. The
// v1beta1 schema is the v1alpha1 schema field for field (design book, API
// Versioning and Deprecation), so conversion in both directions is a
// structural copy. The round-trip fuzz suite is what keeps that honest: the
// day a field differs between the versions, the identity breaks for that
// kind and its conversion becomes a hand-written rule.

// convertViaJSON copies src into dst through their JSON forms and then stamps
// dst with gvk. The stamp matters: the controller-runtime handler sets the
// destination's apiVersion and kind before calling ConvertTo or ConvertFrom,
// and the copy would otherwise overwrite them with the source's.
func convertViaJSON(src, dst runtime.Object, gvk schema.GroupVersionKind) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("marshal %T: %w", src, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("unmarshal into %T: %w", dst, err)
	}
	dst.GetObjectKind().SetGroupVersionKind(gvk)
	return nil
}

func hubKind(kind string) schema.GroupVersionKind { return kaalmv1beta1.GroupVersion.WithKind(kind) }
func spokeKind(kind string) schema.GroupVersionKind {
	return GroupVersion.WithKind(kind)
}

// ConvertTo converts this Agent to the v1beta1 hub.
func (src *Agent) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.Agent)
	if !ok {
		return fmt.Errorf("Agent.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("Agent"))
}

// ConvertFrom converts from the v1beta1 hub to this Agent.
func (dst *Agent) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.Agent)
	if !ok {
		return fmt.Errorf("Agent.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("Agent"))
}

// ConvertTo converts this AgentChannel to the v1beta1 hub.
func (src *AgentChannel) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.AgentChannel)
	if !ok {
		return fmt.Errorf("AgentChannel.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("AgentChannel"))
}

// ConvertFrom converts from the v1beta1 hub to this AgentChannel.
func (dst *AgentChannel) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.AgentChannel)
	if !ok {
		return fmt.Errorf("AgentChannel.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("AgentChannel"))
}

// ConvertTo converts this AgentClass to the v1beta1 hub.
func (src *AgentClass) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.AgentClass)
	if !ok {
		return fmt.Errorf("AgentClass.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("AgentClass"))
}

// ConvertFrom converts from the v1beta1 hub to this AgentClass.
func (dst *AgentClass) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.AgentClass)
	if !ok {
		return fmt.Errorf("AgentClass.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("AgentClass"))
}

// ConvertTo converts this AgentTask to the v1beta1 hub.
func (src *AgentTask) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.AgentTask)
	if !ok {
		return fmt.Errorf("AgentTask.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("AgentTask"))
}

// ConvertFrom converts from the v1beta1 hub to this AgentTask.
func (dst *AgentTask) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.AgentTask)
	if !ok {
		return fmt.Errorf("AgentTask.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("AgentTask"))
}

// ConvertTo converts this ModelProvider to the v1beta1 hub.
func (src *ModelProvider) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.ModelProvider)
	if !ok {
		return fmt.Errorf("ModelProvider.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("ModelProvider"))
}

// ConvertFrom converts from the v1beta1 hub to this ModelProvider.
func (dst *ModelProvider) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.ModelProvider)
	if !ok {
		return fmt.Errorf("ModelProvider.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("ModelProvider"))
}

// ConvertTo converts this ToolProvider to the v1beta1 hub.
func (src *ToolProvider) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*kaalmv1beta1.ToolProvider)
	if !ok {
		return fmt.Errorf("ToolProvider.ConvertTo: unexpected hub type %T", dstRaw)
	}
	return convertViaJSON(src, dst, hubKind("ToolProvider"))
}

// ConvertFrom converts from the v1beta1 hub to this ToolProvider.
func (dst *ToolProvider) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*kaalmv1beta1.ToolProvider)
	if !ok {
		return fmt.Errorf("ToolProvider.ConvertFrom: unexpected hub type %T", srcRaw)
	}
	return convertViaJSON(src, dst, spokeKind("ToolProvider"))
}
