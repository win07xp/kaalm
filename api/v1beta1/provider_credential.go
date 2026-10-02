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

import "net/url"

// The provider credential checks of rules 49 and 50. The ModelProvider and
// ToolProvider reconcilers and the gateway all call these, so the controller
// and the gateway cannot disagree on which Secret a provider may use or where
// its credential may go.

// ProviderCredentialOptedIn reports whether a Secret with these labels opts in
// to ModelProvider and ToolProvider credentialsRef use: LabelProviderCredential
// set to exactly AnnotationTrue (rule 49). Other labels, such as
// LabelChannelCredential, do not count.
func ProviderCredentialOptedIn(labels map[string]string) bool {
	return labels[LabelProviderCredential] == AnnotationTrue
}

// EndpointHost returns the hostname of a provider spec.endpoint: the host the
// gateway and the probes dial, without port, IPv6 brackets, or userinfo. It
// returns "" when the endpoint does not parse, is not https, or has no host.
func EndpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return u.Hostname()
}

// ProviderHostApproved reports whether a provider credential Secret with
// these annotations may be sent to host (rule 50). AnnotationProviderHosts is
// a comma-separated list of hostnames, matched exactly as rule 46 matches
// AnnotationCallbackHosts: an entry matches when it equals host, compared
// without case and after trimming spaces, with no wildcards and no suffix
// matches. AnnotationCallbackHosts does not count.
func ProviderHostApproved(annotations map[string]string, host string) bool {
	return hostListed(annotations[AnnotationProviderHosts], host)
}
