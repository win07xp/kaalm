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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoCRDs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agents.kaalm.io
spec:
  group: kaalm.io
  names:
    plural: agents
  conversion:
    strategy: Webhook
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agentclasses.kaalm.io
spec:
  group: kaalm.io
  names:
    plural: agentclasses
`

func TestRunSplitsPerCRDWithControllerGenNames(t *testing.T) {
	dir := t.TempDir()
	if err := run(strings.NewReader(twoCRDs), dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kaalm.io_agents.yaml", "kaalm.io_agentclasses.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(string(b), "---\napiVersion: apiextensions.k8s.io/v1\n") {
			t.Fatalf("%s does not start with the document marker:\n%s", name, b)
		}
		if strings.Count(string(b), "kind: CustomResourceDefinition") != 1 {
			t.Fatalf("%s holds more than one document:\n%s", name, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kaalm.io_agents.yaml")); !strings.Contains(string(b), "strategy: Webhook") {
		t.Fatalf("conversion stanza lost:\n%s", b)
	}
}

func TestRunRejectsNonCRDAndEmptyInput(t *testing.T) {
	dir := t.TempDir()
	if err := run(strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), dir); err == nil {
		t.Fatal("a non-CRD document was accepted")
	}
	if err := run(strings.NewReader("---\n"), dir); err == nil {
		t.Fatal("empty input was accepted")
	}
	if err := run(strings.NewReader("not: [valid"), dir); err == nil {
		t.Fatal("unparsable input was accepted")
	}
}
