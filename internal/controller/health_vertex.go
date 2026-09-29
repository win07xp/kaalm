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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/win07xp/kaalm/internal/gcpauth"
)

// vertexRegionalHostSuffix is the host suffix of a regional Vertex endpoint,
// {location}-aiplatform.googleapis.com.
const vertexRegionalHostSuffix = "-aiplatform.googleapis.com"

// vertexProbeRequest builds the google-vertex liveness request: a GET of the
// publisher-models list,
// {endpoint}/v1/projects/{project}/locations/{location}/publishers/google/models,
// carrying an OAuth2 access token minted from the service-account JSON key
// in the credential. The project is the key's project_id and the location
// comes from the endpoint host (vertexLocation). The token exchange runs
// over hc, so it shares the probe's timeout, redirect refusal, and trust
// pool. A credential that is not a usable key, or a token endpoint that
// refuses it, returns an AuthFailed result; any other token failure is
// transient.
func vertexProbeRequest(
	ctx context.Context, hc *http.Client, endpoint, credential string,
) (*http.Request, *ProviderProbeResult) {
	key, err := gcpauth.ParseServiceAccountKey([]byte(credential))
	if err != nil {
		return nil, &ProviderProbeResult{AuthFailed: true, Err: err}
	}
	token, err := key.AccessToken(ctx, hc)
	if err != nil {
		if errors.Is(err, gcpauth.ErrRejected) {
			return nil, &ProviderProbeResult{AuthFailed: true, Err: err}
		}
		return nil, &ProviderProbeResult{Err: fmt.Errorf("minting the Vertex access token: %w", err)}
	}
	target := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models",
		strings.TrimSuffix(endpoint, "/"), url.PathEscape(key.ProjectID), url.PathEscape(vertexLocation(endpoint)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &ProviderProbeResult{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// vertexLocation derives the Vertex location from the endpoint: the
// {location} of a regional {location}-aiplatform.googleapis.com host, and
// "global" for any other host, including the global
// aiplatform.googleapis.com endpoint.
func vertexLocation(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "global"
	}
	if loc, ok := strings.CutSuffix(u.Hostname(), vertexRegionalHostSuffix); ok && loc != "" {
		return loc
	}
	return "global"
}
