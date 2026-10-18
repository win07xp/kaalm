// Command gateway is the Kaalm Gateway: the cluster listener on :8443 (the
// LLM proxy, the MCP tool broker, and the internal endpoints, with per-path
// client authentication), the Ingress-fronted user listener on :8080, and a
// dedicated health port. See docs/src/gateways/.
package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func TestParseBackoff(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []time.Duration
	}{
		{
			name: "empty string returns nil (Config default)",
			raw:  "",
			want: nil,
		},
		{
			name: "single duration",
			raw:  "1s",
			want: []time.Duration{time.Second},
		},
		{
			name: "typical backoff schedule",
			raw:  "1s,5s,25s",
			want: []time.Duration{time.Second, 5 * time.Second, 25 * time.Second},
		},
		{
			name: "entries with surrounding whitespace are trimmed",
			raw:  " 1s , 5s ,25s ",
			want: []time.Duration{time.Second, 5 * time.Second, 25 * time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBackoff(tt.raw)
			if err != nil {
				t.Fatalf("parseBackoff(%q) errored: %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseBackoff(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}

	// A malformed entry is a startup error, never a silently shortened
	// schedule.
	for _, raw := range []string{"1s,not-a-duration,25s", "garbage"} {
		if _, err := parseBackoff(raw); err == nil {
			t.Errorf("parseBackoff(%q) must error", raw)
		}
	}
}

func TestValidatePlatformBaseURL(t *testing.T) {
	for _, ok := range []string{
		"https://discord.com/api/v10",
		"https://graph.facebook.com/v23.0",
		"http://mockdiscord.kaalm-e2e.svc:8080",
	} {
		if err := validatePlatformBaseURL(ok); err != nil {
			t.Errorf("validatePlatformBaseURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"discord.com/api",        // no scheme
		"ftp://discord.com",      // wrong scheme
		"https://",               // no host
		"https://h.example?x=1",  // query
		"https://h.example#frag", // fragment
		"https://h .example/api", // unparseable
	} {
		if err := validatePlatformBaseURL(bad); err == nil {
			t.Errorf("validatePlatformBaseURL(%q) must error", bad)
		}
	}
}

// The request-path kinds are served from the cache without a deep copy;
// the options that keep Secrets out of the cache and scope ConfigMaps
// stay as they were.
type limitCase struct {
	name      string
	counts    map[string]int64
	durations map[string]time.Duration
	// wantFlags lists the flags the error must name; empty means no error.
	wantFlags []string
}

func TestValidateLimits(t *testing.T) {
	tests := []limitCase{
		{
			name:      "zero keeps the fallback",
			counts:    map[string]int64{"mcp-max-body-bytes": 0},
			durations: map[string]time.Duration{"mcp-upstream-timeout": 0},
		},
		{
			name:      "flag defaults",
			counts:    map[string]int64{"mcp-max-body-bytes": 4 << 20},
			durations: map[string]time.Duration{"mcp-upstream-timeout": 120 * time.Second},
		},
		{
			name:      "negative MCP body cap",
			counts:    map[string]int64{"mcp-max-body-bytes": -1},
			durations: map[string]time.Duration{"mcp-upstream-timeout": 0},
			wantFlags: []string{"--mcp-max-body-bytes"},
		},
		{
			name:      "negative MCP timeout",
			counts:    map[string]int64{"mcp-max-body-bytes": 0},
			durations: map[string]time.Duration{"mcp-upstream-timeout": -5 * time.Second},
			wantFlags: []string{"--mcp-upstream-timeout"},
		},
		{
			name:      "both negative in one error",
			counts:    map[string]int64{"mcp-max-body-bytes": -1},
			durations: map[string]time.Duration{"mcp-upstream-timeout": -time.Second},
			wantFlags: []string{"--mcp-max-body-bytes", "--mcp-upstream-timeout"},
		},
	}
	// The other size, count, and timeout flags main() checks: zero means
	// the NewServer default, and a negative value fails.
	for _, name := range []string{
		"max-llm-body-bytes", "max-message-body-bytes", "max-response-body-bytes", "max-fallback-depth",
	} {
		tests = append(tests, limitCase{
			name: "negative " + name, counts: map[string]int64{name: -1}, wantFlags: []string{"--" + name},
		})
	}
	for _, name := range []string{
		"upstream-timeout", "sync-delivery-deadline", "agent-read-timeout",
		"callback-read-timeout", "agent-connect-timeout", "channel-health-window",
	} {
		tests = append(tests, limitCase{
			name: "negative " + name, durations: map[string]time.Duration{name: -time.Second}, wantFlags: []string{"--" + name},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLimits(tt.counts, tt.durations)
			if len(tt.wantFlags) == 0 {
				if err != nil {
					t.Fatalf("validateLimits: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateLimits returned nil, want an error naming %v", tt.wantFlags)
			}
			for _, flagName := range tt.wantFlags {
				if !strings.Contains(err.Error(), flagName) {
					t.Errorf("error %q does not name %s", err, flagName)
				}
			}
		})
	}
}

func TestClusterOptionsShareRequestPathObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	var o cluster.Options
	clusterOptions(scheme, "kaalm-system")(&o)

	if o.Scheme != scheme {
		t.Error("scheme not set")
	}
	if o.Cache.DefaultTransform == nil {
		t.Error("managedFields transform not set")
	}
	if o.Client.Cache == nil || len(o.Client.Cache.DisableFor) != 1 {
		t.Fatalf("client cache options = %+v, want Secrets read uncached", o.Client.Cache)
	}
	if _, ok := o.Client.Cache.DisableFor[0].(*corev1.Secret); !ok {
		t.Errorf("DisableFor = %T, want *corev1.Secret", o.Client.Cache.DisableFor[0])
	}
	byKind := map[string]cache.ByObject{}
	for obj, cfg := range o.Cache.ByObject {
		byKind[reflect.TypeOf(obj).Elem().Name()] = cfg
	}
	for _, kind := range []string{"Agent", "AgentTask", "AgentClass", "ModelProvider", "ToolProvider"} {
		cfg, ok := byKind[kind]
		if !ok || cfg.UnsafeDisableDeepCopy == nil || !*cfg.UnsafeDisableDeepCopy {
			t.Errorf("%s: deep copy not disabled (%+v)", kind, cfg)
		}
	}
	cm, ok := byKind["ConfigMap"]
	if !ok {
		t.Fatal("ConfigMap has no cache options")
	}
	if _, scoped := cm.Namespaces["kaalm-system"]; !scoped || len(cm.Namespaces) != 1 {
		t.Errorf("ConfigMap namespaces = %v, want the operator namespace only", cm.Namespaces)
	}
	if cm.UnsafeDisableDeepCopy != nil {
		t.Error("ConfigMap reads must keep their deep copy")
	}
}
