// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tracing wires the OpenTelemetry SDK and exposes a small
// HTTP-middleware helper. When tracing is disabled, all helpers are no-ops
// so callers don't need to branch.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// Config controls the SDK setup.
type Config struct {
	Enabled     bool
	Endpoint    string  // OTLP HTTP endpoint, e.g. "http://otel-collector:4318"
	SampleRate  float64 // 0..1
	ServiceName string
}

// Init constructs the tracer provider per cfg and installs it as the
// global OTel provider. Returns a shutdown function the caller invokes on
// exit. When cfg.Enabled is false, returns a no-op shutdown.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("tracing: endpoint required when enabled")
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("tracing: parse endpoint: %w", err)
	}
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(u.Host),
		otlptracehttp.WithURLPath(u.Path),
	}
	if u.Scheme != "https" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
	if err != nil {
		return nil, fmt.Errorf("tracing: exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return provider.Shutdown(shutdownCtx)
	}, nil
}

// Tracer returns a named tracer from the global provider. Safe to call when
// tracing is disabled (returns a no-op tracer).
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Middleware returns HTTP middleware that emits one span per request,
// linking it to the request_id when present.
func Middleware(next http.Handler) http.Handler {
	tracer := Tracer("shithub.web")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(
			r.Context(), r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
			),
		)
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
