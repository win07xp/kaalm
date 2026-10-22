// Command gateway is the Kaalm Gateway: the cluster listener on :8443 (the
// LLM proxy, the MCP tool broker, and the internal endpoints, with per-path
// client authentication), the Ingress-fronted user listener on :8080, and a
// dedicated health port. See docs/src/gateways/.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
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
		"http://mock-discord.e2e.svc:8080/api/v10",
		"http://mock-whatsapp.e2e.svc:8080",
		"https://h.example:443/v1",
		"http://[::1]:8080/api",
		"HTTPS://h.example/v1",
	} {
		if err := validatePlatformBaseURL(ok); err != nil {
			t.Errorf("validatePlatformBaseURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"discord.com/api",                   // no scheme
		"ftp://discord.com",                 // wrong scheme
		"https://",                          // no host
		"https://h.example?x=1",             // query
		"https://h.example#frag",            // fragment
		"https://h .example/api",            // unparseable
		"https://graph.facebook.com/v23.0?", // bare query
		"https://h.example/v1#",             // bare fragment
		"https://h.example#",                // bare fragment, no path
		"https://h.example:99999/v1",        // port too high
		"https://h.example:0/v1",            // port zero
		"https://user:pw@h.example/v1",      // user info
		"https://tok3n@h.example/v1",        // user name only, often a token
		"http://:8080",                      // empty hostname
	} {
		if err := validatePlatformBaseURL(bad); err == nil {
			t.Errorf("validatePlatformBaseURL(%q) must error", bad)
		}
	}
}

func TestValidatePlatformBaseURL_HidesUserInfo(t *testing.T) {
	for _, tc := range []struct{ value, secret string }{
		{"https://user:secret@h.example/v1?x=1", "secret"},
		{"https://user:secret@h.example:99999/v1", "secret"},
		{"https://user:secret@h.example:abc/v1", "secret"}, // url.Parse failure
		{"https://tok3n@h.example/v1#", "tok3n"},
		{"ftp://tok3n@h.example", "tok3n"},
		{"user:secret@h.example/v1", "secret"},
		{"https:tok3n@h.example/v1", "tok3n"},
		// A query or fragment can carry a token too.
		{"https://h.example/v1?token=s3cret", "s3cret"},
		{"https://h.example:99999/v1?token=s3cret", "s3cret"},
		{"https://h.example/v1#s3cret", "s3cret"},
		{"ftp://h.example/v1#s3cret", "s3cret"},
		{"h.example/v1?token=s3cret", "s3cret"},
	} {
		err := validatePlatformBaseURL(tc.value)
		if err == nil {
			t.Errorf("validatePlatformBaseURL(%q) must error", tc.value)
			continue
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Errorf("validatePlatformBaseURL(%q) error %q leaks %q", tc.value, err, tc.secret)
		}
	}
}

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

// Every size, count, and timeout flag that main() checks, as a user passes
// it. A row fails if limitFlags leaves its flag out or misspells its name.
var limitFlagNames = []string{
	"max-llm-body-bytes", "max-message-body-bytes", "max-response-body-bytes",
	"max-fallback-depth", "mcp-max-body-bytes",
	"upstream-timeout", "sync-delivery-deadline", "agent-read-timeout",
	"callback-read-timeout", "agent-connect-timeout", "channel-health-window",
	"mcp-upstream-timeout",
}

// parseLimitFlag parses --name=value into a fresh FlagSet, using a duration
// value for a duration flag, and returns validateLimits' result.
func parseLimitFlag(t *testing.T, name string, count int64) error {
	t.Helper()
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	f := registerFlags(fs)
	fl := fs.Lookup(name)
	if fl == nil {
		t.Fatalf("no --%s flag registered", name)
	}
	value := strconv.FormatInt(count, 10)
	if _, ok := fl.Value.(flag.Getter).Get().(time.Duration); ok {
		value += "s"
	}
	if err := fs.Parse([]string{"--" + name + "=" + value}); err != nil {
		t.Fatalf("parsing --%s=%s: %v", name, value, err)
	}
	return validateLimits(limitFlags(f))
}

func TestLimitFlagsRejectNegativeFlag(t *testing.T) {
	for _, name := range limitFlagNames {
		t.Run(name, func(t *testing.T) {
			err := parseLimitFlag(t, name, -1)
			if err == nil {
				t.Fatalf("--%s=-1 passed validateLimits, want an error", name)
			}
			if !strings.Contains(err.Error(), "--"+name+" ") {
				t.Errorf("error %q does not name --%s", err, name)
			}
			if !strings.Contains(err.Error(), "-1") {
				t.Errorf("error %q does not show the value", err)
			}
		})
	}
}

func TestLimitFlagsAcceptZeroAndDefaults(t *testing.T) {
	for _, name := range limitFlagNames {
		if err := parseLimitFlag(t, name, 0); err != nil {
			t.Errorf("--%s=0: %v, want nil (zero keeps the fallback)", name, err)
		}
	}
	f := registerFlags(flag.NewFlagSet("gateway", flag.ContinueOnError))
	if err := validateLimits(limitFlags(f)); err != nil {
		t.Errorf("flag defaults: %v, want nil", err)
	}
}

// A zero or negative wake cap is refused at startup.
func TestValidateMaxWakeTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if err := validateMaxWakeTimeout(d); err == nil {
			t.Errorf("validateMaxWakeTimeout(%v) = nil, want an error", d)
		}
	}
	for _, d := range []time.Duration{time.Second, 15 * time.Minute} {
		if err := validateMaxWakeTimeout(d); err != nil {
			t.Errorf("validateMaxWakeTimeout(%v) = %v, want nil", d, err)
		}
	}
}

// The request-path kinds are served from the cache without a deep copy;
// the options that keep Secrets out of the cache and scope ConfigMaps
// stay as they were.
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

// The Pod wait's tracker rides on the request path's Pod informer and counts
// as synced through its handler registration; when the informer is not
// available the tracker stays unsynced, so waits poll.
func TestPodReadyTrackerUsesThePodInformer(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	if tr := podReadyTracker(&informertest.FakeInformers{}, logger); tr.Synced == nil {
		t.Error("tracker has no sync check after registering on the Pod informer")
	}
	failing := &informertest.FakeInformers{Error: errors.New("no Pod informer")}
	if tr := podReadyTracker(failing, logger); tr == nil || tr.Synced != nil {
		t.Errorf("tracker = %+v, want one with no sync check when the informer is unavailable", tr)
	}
}
