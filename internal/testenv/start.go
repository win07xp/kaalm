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

// Package testenv starts envtest control planes for the test suites.
//
// Start differs from envtest.Environment.Start in two ways. First, the admin
// user's client certificate and its CA are valid from an hour before they
// were minted. envtest stamps NotBefore with the mint time and the apiserver
// checks the certificate on every request at its own wall clock, so a clock
// stepped backward right after minting turns every admin request into 401
// Unauthorized. WSL2 does exactly that when systemd-timesyncd and the Hyper-V
// time sync disagree: the clock steps back a second or two about every 30
// seconds. Second, a start that fails after the control plane came
// up stops it, so the kube-apiserver and etcd do not outlive the test binary.
package testenv

import (
	"errors"
	"fmt"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Start starts env with backdated admin credentials. On failure it stops
// whatever part of the control plane came up and returns the start error.
func Start(env *envtest.Environment) (*rest.Config, error) {
	apiServer := env.ControlPlane.GetAPIServer()
	if apiServer.Authn == nil {
		authn, err := newBackdatedCertAuthn()
		if err != nil {
			return nil, err
		}
		apiServer.Authn = authn
	}
	cfg, err := env.Start()
	if err == nil {
		return cfg, nil
	}
	// envtest leaves the control plane running when a later step fails
	// (provisioning the admin user, installing CRDs or webhooks). CertDir is
	// set once the apiserver was configured, which needs etcd running; an
	// earlier failure has nothing to stop, and envtest's Stop would
	// dereference process state that does not exist yet.
	if apiServer.CertDir != "" {
		if stopErr := env.Stop(); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop envtest after failed start: %w", stopErr))
		}
	}
	return nil, err
}
