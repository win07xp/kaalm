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
