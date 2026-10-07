// Package telemetry sets up OpenTelemetry tracing. It is off unless an OTLP
// endpoint is configured through the standard OTEL_EXPORTER_OTLP_ENDPOINT
// variable, and costs nothing when off. When on, one attempt is one trace:
// the scheduler's dispatch carries the trace context to the agent, so placing,
// dispatching, preflight, measurement, and upload line up on one timeline
// across two processes.
package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const scope = "github.com/rajeev-chaurasia/benchgrid"

// Setup installs the propagator unconditionally, so trace context passes
// through a process that does not export, and installs an exporter only when
// an endpoint is configured. The returned function flushes and stops it.
func Setup(ctx context.Context, service string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func Tracer() trace.Tracer { return otel.Tracer(scope) }
