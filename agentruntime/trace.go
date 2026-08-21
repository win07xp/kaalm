// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"net/http"
)

// traceContextKey carries the W3C trace context of the message being handled.
type traceContextKey struct{}

type traceContext struct{ parent, state string }

// TraceContext returns the W3C trace context the gateway attached to the
// delivery of the message being handled (the "traceparent" and "tracestate"
// header values), or empty strings when the delivery carried none. The
// runtime already forwards these on every Gateway call made with this ctx;
// handlers running their own OpenTelemetry SDK can continue the trace from
// them (runtime contract item 8).
func TraceContext(ctx context.Context) (traceparent, tracestate string) {
	tc, _ := ctx.Value(traceContextKey{}).(traceContext)
	return tc.parent, tc.state
}

func withTraceContext(ctx context.Context, parent, state string) context.Context {
	return context.WithValue(ctx, traceContextKey{}, traceContext{parent: parent, state: state})
}

// applyTraceContext copies the handled message's trace context onto an
// outbound gateway request, so the LLM and tool spans the gateway creates
// stay children of the delivery that caused them.
func applyTraceContext(ctx context.Context, req *http.Request) {
	parent, state := TraceContext(ctx)
	if parent == "" {
		return
	}
	req.Header.Set("traceparent", parent)
	if state != "" {
		req.Header.Set("tracestate", state)
	}
}
