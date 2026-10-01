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

package chart

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// probeTrust is what the chart gives the controller for provider health
// probe TLS: the --probe-ca file list and the extra CA ConfigMaps projected
// into the kaalm-tls volume, as name/key -> path.
type probeTrust struct {
	files      []string // nil when the controller gets no --probe-ca flag
	configMaps map[string]string
}

func renderProbeTrust(t *testing.T, args ...string) probeTrust {
	t.Helper()
	out := helmTemplate(t, "controller.yaml", args...)
	for _, doc := range strings.Split(out, "\n---\n") {
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || d.Kind != "Deployment" {
			continue
		}
		pt := probeTrust{configMaps: map[string]string{}}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name != "manager" {
				continue
			}
			for _, a := range c.Args {
				if v, ok := strings.CutPrefix(a, "--probe-ca="); ok {
					if pt.files != nil {
						t.Fatalf("--probe-ca appears more than once: %v", c.Args)
					}
					pt.files = strings.Split(v, ",")
				}
			}
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name != "kaalm-tls" || v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ConfigMap == nil || s.ConfigMap.Name == "kaalm-ca-system" {
					continue
				}
				for _, it := range s.ConfigMap.Items {
					pt.configMaps[s.ConfigMap.Name+"/"+it.Key] = it.Path
				}
			}
		}
		return pt
	}
	t.Fatalf("controller.yaml rendered no Deployment\n%s", out)
	return probeTrust{}
}

func wantProbeFiles(t *testing.T, pt probeTrust, want ...string) {
	t.Helper()
	got := slices.Clone(pt.files)
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("--probe-ca files = %v, want %v", pt.files, want)
	}
}

func wantProjected(t *testing.T, pt probeTrust, want map[string]string) {
	t.Helper()
	if len(pt.configMaps) != len(want) {
		t.Errorf("projected CA ConfigMaps = %v, want %v", pt.configMaps, want)
		return
	}
	for k, p := range want {
		if pt.configMaps[k] != p {
			t.Errorf("projected CA ConfigMaps = %v, want %v", pt.configMaps, want)
			return
		}
	}
}

const (
	clusterCAPath  = "/var/run/kaalm/ca.crt"
	upstreamCAPath = "/var/run/kaalm/upstream-ca.crt"
	probeCAPath    = "/var/run/kaalm/probe-ca.crt"
)

// Without any trust values the controller probes with the system roots only.
func TestControllerProbeCA_Defaults(t *testing.T) {
	pt := renderProbeTrust(t)
	if pt.files != nil {
		t.Errorf("--probe-ca = %v, want no flag", pt.files)
	}
	wantProjected(t, pt, map[string]string{})
}

// The gateway's upstream trust values alone give the controller's probes the
// same trust the forwarding path has.
func TestControllerProbeCA_FollowsGatewayValues(t *testing.T) {
	pt := renderProbeTrust(t,
		"--set", "gateway.trustClusterCAForUpstream=true",
		"--set", "gateway.upstreamCA.configMap=corp-ca",
		"--set", "gateway.upstreamCA.key=bundle.pem",
	)
	wantProbeFiles(t, pt, clusterCAPath, upstreamCAPath)
	wantProjected(t, pt, map[string]string{"corp-ca/bundle.pem": "upstream-ca.crt"})
}

// Each gateway value works on its own.
func TestControllerProbeCA_GatewayValuesSeparately(t *testing.T) {
	pt := renderProbeTrust(t, "--set", "gateway.trustClusterCAForUpstream=true")
	wantProbeFiles(t, pt, clusterCAPath)
	wantProjected(t, pt, map[string]string{})

	pt = renderProbeTrust(t, "--set", "gateway.upstreamCA.configMap=corp-ca")
	wantProbeFiles(t, pt, upstreamCAPath)
	wantProjected(t, pt, map[string]string{"corp-ca/ca.crt": "upstream-ca.crt"})
}

// The deprecated controller values still work on their own, so existing
// values files keep their probe trust.
func TestControllerProbeCA_DeprecatedValuesStillHonored(t *testing.T) {
	pt := renderProbeTrust(t,
		"--set", "controller.trustClusterCAForProbes=true",
		"--set", "controller.probeCA.configMap=probe-ca",
	)
	wantProbeFiles(t, pt, clusterCAPath, probeCAPath)
	wantProjected(t, pt, map[string]string{"probe-ca/ca.crt": "probe-ca.crt"})
}

// Gateway and deprecated values merge into one additive pool. The cluster CA
// is listed once, a distinct deprecated bundle is kept, and a deprecated
// bundle naming the same ConfigMap key as the gateway's is projected once.
func TestControllerProbeCA_MergesWithoutDuplicates(t *testing.T) {
	both := []string{
		"--set", "gateway.trustClusterCAForUpstream=true",
		"--set", "controller.trustClusterCAForProbes=true",
		"--set", "gateway.upstreamCA.configMap=corp-ca",
	}

	pt := renderProbeTrust(t, append(slices.Clone(both), "--set", "controller.probeCA.configMap=old-ca")...)
	wantProbeFiles(t, pt, clusterCAPath, upstreamCAPath, probeCAPath)
	wantProjected(t, pt, map[string]string{
		"corp-ca/ca.crt": "upstream-ca.crt",
		"old-ca/ca.crt":  "probe-ca.crt",
	})

	pt = renderProbeTrust(t, append(slices.Clone(both), "--set", "controller.probeCA.configMap=corp-ca")...)
	wantProbeFiles(t, pt, clusterCAPath, upstreamCAPath)
	wantProjected(t, pt, map[string]string{"corp-ca/ca.crt": "upstream-ca.crt"})
}
