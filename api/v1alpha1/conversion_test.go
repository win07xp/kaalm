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
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
	webhookconversion "sigs.k8s.io/controller-runtime/pkg/webhook/conversion"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// otherHub is a hub of the wrong kind, to prove the spokes refuse it instead
// of copying into it.
type otherHub struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

func (*otherHub) Hub()                             {}
func (o *otherHub) DeepCopyObject() runtime.Object { return o }

// spoke is what every v1alpha1 root type satisfies: a runtime.Object that
// converts to and from the hub.
type spoke interface {
	runtime.Object
	conversion.Convertible
}

// roundTrip converts a populated spoke to the hub and back, and asserts that
// the hub carries the v1beta1 apiVersion and kind, that the round trip is the
// identity on the spoke, and that the hub's spec and status are byte-identical
// to the spoke's in JSON (the structural-copy contract).
func roundTrip(t *testing.T, kind string, src spoke, hub conversion.Hub, back spoke) {
	t.Helper()
	t.Run(kind, func(t *testing.T) {
		if err := src.ConvertTo(hub); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		if got, want := hub.GetObjectKind().GroupVersionKind(), kaalmv1beta1.GroupVersion.WithKind(kind); got != want {
			t.Fatalf("hub GVK = %v, want %v", got, want)
		}
		if err := back.ConvertFrom(hub); err != nil {
			t.Fatalf("ConvertFrom: %v", err)
		}
		if got, want := back.GetObjectKind().GroupVersionKind(), GroupVersion.WithKind(kind); got != want {
			t.Fatalf("spoke GVK = %v, want %v", got, want)
		}
		if !equality.Semantic.DeepEqual(src, back) {
			t.Fatalf("round trip is not the identity\nsrc:  %#v\nback: %#v", src, back)
		}
		srcJSON, err := json.Marshal(src)
		if err != nil {
			t.Fatal(err)
		}
		hubJSON, err := json.Marshal(hub)
		if err != nil {
			t.Fatal(err)
		}
		var a, b map[string]json.RawMessage
		if err := json.Unmarshal(srcJSON, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(hubJSON, &b); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"spec", "status", "metadata"} {
			if string(a[field]) != string(b[field]) {
				t.Fatalf("%s differs between spoke and hub\nspoke: %s\nhub:   %s", field, a[field], b[field])
			}
		}
	})
}

func TestConversionRoundTrip(t *testing.T) {
	roundTrip(t, "Agent", newFullAgent(), &kaalmv1beta1.Agent{}, &Agent{})
	roundTrip(t, "AgentChannel", newFullAgentChannel(), &kaalmv1beta1.AgentChannel{}, &AgentChannel{})
	roundTrip(t, "AgentClass", newFullAgentClass(), &kaalmv1beta1.AgentClass{}, &AgentClass{})
	roundTrip(t, "AgentTask", newFullAgentTask(), &kaalmv1beta1.AgentTask{}, &AgentTask{})
	roundTrip(t, "ModelProvider", newFullModelProvider(), &kaalmv1beta1.ModelProvider{}, &ModelProvider{})
	roundTrip(t, "ToolProvider", newFullToolProvider(), &kaalmv1beta1.ToolProvider{}, &ToolProvider{})
}

// TestConversionStampsDestinationGVK proves the stamp: a source whose TypeMeta
// names the other version must not leak it into the destination, and a
// destination with an empty TypeMeta ends up with its own version regardless.
func TestConversionStampsDestinationGVK(t *testing.T) {
	src := newFullAgent()
	hub := &kaalmv1beta1.Agent{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatal(err)
	}
	if hub.APIVersion != "kaalm.io/v1beta1" || hub.Kind != "Agent" {
		t.Fatalf("hub TypeMeta = %s/%s, want kaalm.io/v1beta1/Agent", hub.APIVersion, hub.Kind)
	}
	back := &Agent{}
	if err := back.ConvertFrom(hub); err != nil {
		t.Fatal(err)
	}
	if back.APIVersion != "kaalm.io/v1alpha1" || back.Kind != "Agent" {
		t.Fatalf("spoke TypeMeta = %s/%s, want kaalm.io/v1alpha1/Agent", back.APIVersion, back.Kind)
	}
}

func TestConversionRefusesWrongHub(t *testing.T) {
	wrong := &otherHub{}
	for _, s := range []spoke{&Agent{}, &AgentChannel{}, &AgentClass{}, &AgentTask{}, &ModelProvider{}, &ToolProvider{}} {
		if err := s.ConvertTo(wrong); err == nil {
			t.Errorf("%T.ConvertTo(otherHub) returned nil error", s)
		}
		if err := s.ConvertFrom(wrong); err == nil {
			t.Errorf("%T.ConvertFrom(otherHub) returned nil error", s)
		}
	}
}

// TestConversionHubAndSpokeRegistered proves a scheme holding both versions
// sees every kind as convertible, which is the condition under which
// controller-runtime serves conversion (and envtest installs it).
func TestConversionHubAndSpokeRegistered(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []runtime.Object{&Agent{}, &AgentChannel{}, &AgentClass{}, &AgentTask{}, &ModelProvider{}, &ToolProvider{}} {
		ok, err := webhookconversion.IsConvertible(s, obj)
		if err != nil || !ok {
			t.Errorf("%T: IsConvertible = %v, %v; want true, nil", obj, ok, err)
		}
	}
}
