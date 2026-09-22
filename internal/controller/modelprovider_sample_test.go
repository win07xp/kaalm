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

package controller

import (
	"os"
	"testing"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/yaml"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The sample ModelProvider's degrade policy must change the model: its
// target is a valid catalog entry, the cheapest one, and not the only one.
func TestModelProviderSample_DegradeIsEffective(t *testing.T) {
	raw, err := os.ReadFile("../../config/samples/kaalm_v1beta1_modelprovider.yaml")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	var mp kaalmv1beta1.ModelProvider
	if err := yaml.UnmarshalStrict(raw, &mp); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	if problems := validateDegradeTargets(&mp); len(problems) > 0 {
		t.Fatalf("sample degrade targets invalid: %v", problems)
	}
	degrades := 0
	for _, p := range mp.Spec.Budget.Policies {
		if p.Action != kaalmv1beta1.BudgetActionDegrade || p.DegradeTo == nil {
			continue
		}
		degrades++
		others := 0
		for _, m := range mp.Spec.Models {
			if m.ID != *p.DegradeTo {
				others++
			}
		}
		if others == 0 {
			t.Errorf("degradeTo %q is the catalog's only model, so the policy changes nothing", *p.DegradeTo)
		}
	}
	if degrades == 0 {
		t.Fatal("the sample has no degrade policy")
	}
	rec := record.NewFakeRecorder(4)
	(&ModelProviderReconciler{Recorder: rec}).costSanity(&mp)
	if n := len(rec.Events); n != 0 {
		t.Errorf("the sample's degrade target is not the cheapest model: %s", <-rec.Events)
	}
}
