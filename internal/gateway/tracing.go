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

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracing is the gateway's OpenTelemetry wiring: one tracer, the W3C
// trace-context propagator, and the OTLP exporter behind it
// (docs/src/operations/observability.md#tracing). A nil *Tracing no-ops
// every method, which is the default install: no tracer is installed, no
// trace context is created or forwarded, and request handling behaves
// exactly as it did before tracing existed.
type Tracing struct {
	tracer trace.Tracer
	prop   propagation.TextMapPropagator
	tp     *sdktrace.TracerProvider
}

// NewTracing builds the exporter-backed tracer. endpoint is the OTLP/HTTP
// base URL (for example http://collector:4318); it must be an http or https
// URL with a host, an optional port in 1-65535, and an optional path, with no
// user info, query, or fragment. Any other value returns an error, because
// the exporter would otherwise log the bad value and send to its own default,
// localhost:4318, or the export path built below would break. An https endpoint verifies against pool when non-nil (the
// gateway's upstream trust pool), else the system roots. sampleRatio drives
// parent-based head sampling for traces this gateway starts; propagated
// decisions are honored either way.
func NewTracing(ctx context.Context, endpoint string, sampleRatio float64, pool *x509.CertPool, podName string) (*Tracing, error) {
	u, err := parseOTLPEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(strings.TrimSuffix(endpoint, "/") + "/v1/traces"),
	}
	if u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	} else if pool != nil {
		opts = append(opts, otlptracehttp.WithTLSClientConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	attrs := []attribute.KeyValue{semconv.ServiceName("kaalm-gateway")}
	if podName != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(podName))
	}
	// Schemaless, so the merge takes the default resource's schema URL: two
	// different URLs fail the merge, and the SDK moves its own with each
	// release.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
		sdktrace.WithResource(res),
	)
	return &Tracing{tracer: tp.Tracer("kaalm-gateway"), prop: propagation.TraceContext{}, tp: tp}, nil
}

// parseOTLPEndpoint checks that endpoint is an http or https URL with a host
// and a valid port, and that /v1/traces can be appended to it: user info is
// dropped by the exporter, and a query or fragment (even a bare ? or #) would
// swallow the appended path. No error echoes a password in the value: a
// parse failure reports only the inner cause, and the shape errors print the
// redacted URL.
func parseOTLPEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("otlp endpoint: %w", ue.Err)
		}
		return nil, fmt.Errorf("otlp endpoint: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("otlp endpoint %q: need http(s)://host[:port][/path]", u.Redacted())
	}
	// u.Fragment is empty for a bare #, which still breaks the appended path.
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") {
		return nil, fmt.Errorf("otlp endpoint %q: need http(s)://host[:port][/path] with no user info, query, or fragment", u.Redacted())
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("otlp endpoint %q: port %s is outside 1-65535", u.Redacted(), p)
		}
	}
	return u, nil
}

// Shutdown flushes buffered spans.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.tp.Shutdown(ctx)
}

// Extract returns ctx joined with the W3C trace context in h, when present.
func (t *Tracing) Extract(ctx context.Context, h http.Header) context.Context {
	if t == nil {
		return ctx
	}
	return t.prop.Extract(ctx, propagation.HeaderCarrier(h))
}

// Inject writes ctx's span context into h as traceparent and tracestate.
func (t *Tracing) Inject(ctx context.Context, h http.Header) {
	if t == nil {
		return
	}
	t.prop.Inject(ctx, propagation.HeaderCarrier(h))
}

// Start opens a span and returns the derived context plus an end function
// that records an error status when given a non-nil error.
func (t *Tracing) Start(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, func(err error)) {
	if t == nil {
		return ctx, func(error) {}
	}
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// Detach returns a fresh context carrying only ctx's span identity, for work
// that outlives the request (the async delivery goroutine): the child spans
// stay connected without inheriting the request's cancellation.
func (t *Tracing) Detach(ctx context.Context) context.Context {
	if t == nil {
		return context.Background()
	}
	return trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
}

// spanError marks the span in ctx failed without ending it; the noop span on
// untraced requests makes this safe everywhere.
func spanError(ctx context.Context, errType string) {
	trace.SpanFromContext(ctx).SetStatus(codes.Error, errType)
}
