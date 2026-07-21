package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// TestHTTPProbe_HonorsHealthCheckTimeout proves the probe applies
// healthCheck.timeoutSeconds: a 1s timeout against a 3s server must fail fast.
func TestHTTPProbe_HonorsHealthCheckTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker := &HTTPProviderHealthChecker{}
	provider := &agentryv1alpha1.ModelProvider{
		Spec: agentryv1alpha1.ModelProviderSpec{
			Type:     "openai",
			Endpoint: srv.URL,
			HealthCheck: &agentryv1alpha1.ModelProviderHealthCheck{
				Enabled:        true,
				TimeoutSeconds: 1,
			},
		},
	}

	start := time.Now()
	res := checker.Probe(context.Background(), provider, "sk-test")
	elapsed := time.Since(start)

	if res.Err == nil {
		t.Fatalf("timeoutSeconds=1 vs a 3s server: expected a timeout error, got %+v", res)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("probe ignored the 1s timeout; it took %s", elapsed)
	}
}

// TestHTTPProbe_HealthyWithinTimeout guards the default-timeout path: a nil
// HealthCheck block still probes and a fast server is Healthy.
func TestHTTPProbe_HealthyWithinTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker := &HTTPProviderHealthChecker{}
	provider := &agentryv1alpha1.ModelProvider{
		Spec: agentryv1alpha1.ModelProviderSpec{
			Type:     "openai",
			Endpoint: srv.URL,
		},
	}
	res := checker.Probe(context.Background(), provider, "sk-test")
	if !res.Healthy {
		t.Fatalf("fast server with default timeout: expected Healthy, got %+v", res)
	}
}

// ---- HTTPProviderHealthChecker.Probe classification ----

func TestHTTPProbe_Classification(t *testing.T) {
	// anthropic path sets the x-api-key header; a 200 is Healthy.
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer anthropic.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	checker := &HTTPProviderHealthChecker{}
	probe := func(typ, endpoint string) ProviderProbeResult {
		return checker.Probe(context.Background(), &agentryv1alpha1.ModelProvider{
			Spec: agentryv1alpha1.ModelProviderSpec{Type: typ, Endpoint: endpoint},
		}, "sk-test")
	}

	if res := probe("anthropic", anthropic.URL); !res.Healthy {
		t.Errorf("anthropic 200 should be Healthy: %+v", res)
	}
	if res := probe("openai", unauthorized.URL); !res.AuthFailed {
		t.Errorf("401 should be AuthFailed: %+v", res)
	}
	if res := probe("openai-compatible", serverErr.URL); res.Err == nil {
		t.Errorf("500 should be a transient Err: %+v", res)
	}
	if res := probe("google-vertex", anthropic.URL); !res.Skipped {
		t.Errorf("google-vertex should be Skipped: %+v", res)
	}
	if res := probe("mystery-provider", anthropic.URL); res.Err == nil {
		t.Errorf("unknown type should error: %+v", res)
	}
}
