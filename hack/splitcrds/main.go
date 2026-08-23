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

// splitcrds reads a multi-document YAML stream of CustomResourceDefinitions
// on stdin (the output of `kustomize build config/crd`) and writes one file
// per CRD into the directory given as its only argument, named
// <group>_<plural>.yaml, which is controller-gen's own naming and the layout
// the Helm chart's crds/ directory has always had.
//
// Usage: kustomize build config/crd | go run ./hack/splitcrds charts/kaalm/crds
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

type crdHeader struct {
	Kind string `json:"kind"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Plural string `json:"plural"`
		} `json:"names"`
	} `json:"spec"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: splitcrds <output-dir> < crds.yaml")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "splitcrds:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, dir string) error {
	raw, err := io.ReadAll(bufio.NewReader(in))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	n := 0
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		doc = strings.TrimSpace(strings.TrimPrefix(doc, "---\n"))
		if doc == "" {
			continue
		}
		var h crdHeader
		if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
			return fmt.Errorf("parse document %d: %w", n+1, err)
		}
		if h.Kind != "CustomResourceDefinition" || h.Spec.Group == "" || h.Spec.Names.Plural == "" {
			return fmt.Errorf("document %d is not a CRD with a group and plural name (kind %q)", n+1, h.Kind)
		}
		name := filepath.Join(dir, h.Spec.Group+"_"+h.Spec.Names.Plural+".yaml")
		if err := os.WriteFile(name, []byte("---\n"+doc+"\n"), 0o644); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("no CRD documents on stdin")
	}
	return nil
}
