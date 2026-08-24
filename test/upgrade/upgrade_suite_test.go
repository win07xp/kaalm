//go:build upgrade

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

// Package upgrade holds the S21 upgrade e2e: install the previous released
// chart on a fresh cluster, load it with v1alpha1 workloads, run the two
// documented upgrade steps to the local build, and assert that nothing was
// recreated and nothing was lost. It runs through make e2e-upgrade, never
// as part of make e2e: it owns the release the cluster starts from.
package upgrade

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting kaalm upgrade e2e (S21)\n")
	RunSpecs(t, "kaalm upgrade e2e")
}
