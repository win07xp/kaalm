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
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
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
// base URL (for example http://collector:4318); an https endpoint verifies
// against pool when non-nil (the gateway's upstream trust pool), else the
// system roots. sampleRatio drives parent-based head sampling for traces
// this gateway starts; propagated decisions are honored either way.
func NewTracing(ctx context.Context, endpoint string, sampleRatio float64, pool *x509.CertPool, podName string) (*Tracing, error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(strings.TrimSuffix(endpoint, "/") + "/v1/traces"),
	}
	if strings.HasPrefix(endpoint, "http://") {
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
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...))
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
