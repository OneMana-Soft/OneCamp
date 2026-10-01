package otelInit

import (
	"context"
	"log"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

// collectorEndpoint says how the configured collector address should be
// applied. Exactly one of the two results is set, and both being empty is not
// possible: an unset variable falls back to the local collector.
//
// OTEL_EXPORTER_OTLP_ENDPOINT is defined by the OTLP specification as a URL,
// "https://collector.example.com:4318", scheme included. This code read the
// variable by hand and handed it to WithEndpoint, which is documented as host
// and port ONLY, and then forced WithInsecure on top of it. That is three
// faults at once: passing the option suppresses the variable the SDK would
// otherwise have parsed correctly, a whole URL is used as a hostname, and a
// TLS collector is silently downgraded to plaintext. Nothing reports it,
// because a batch exporter sends its failures to the OTel error handler and
// nothing installs one, so the symptom is simply that no telemetry ever
// arrives.
//
// Both spellings appear in this repository's own compose files, so both keep
// working: "otel-collector:4318" as a bare host and port, and
// "http://otel-collector:4318" as a URL.
func collectorEndpoint() (endpointURL, hostPort string) {
	raw := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	switch {
	case raw == "":
		// Unset is the shipped default. Nothing listens there unless the
		// operator runs the bundled collector, and an exporter with nowhere to
		// send costs a dropped batch rather than anything a user notices.
		return "", "localhost:4318"
	case strings.Contains(raw, "://"):
		return raw, ""
	default:
		return "", raw
	}
}

// exportHeaders carries the collector's credential, when there is one.
//
// Sent only when set. The header used to go out on every request with an empty
// value, which is not the same as not sending it: a collector that authenticates
// sees a present-but-empty credential and rejects the batch, and one that does
// not is being told something meaningless.
func exportHeaders() map[string]string {
	key := strings.TrimSpace(os.Getenv("HYPERDX_INGESTION_API_KEY"))
	if key == "" {
		return nil
	}
	return map[string]string{"authorization": key}
}

// InitTracer initializes the OpenTelemetry tracer and logger.
//
// Telemetry is optional and must never be able to stop the server. This used to
// log.Fatalf when an exporter could not be built, so a typo in a collector
// address took down a chat and video product that was otherwise perfectly able
// to run without sending anyone a trace. It now gives up on telemetry alone.
func InitTracer() func() {
	ctx := context.Background()
	noop := func() {}

	endpointURL, hostPort := collectorEndpoint()
	headers := exportHeaders()

	// --- Trace Exporter ---
	traceOpts := []otlptracehttp.Option{}
	if endpointURL != "" {
		// Host, path and TLS all derived from the URL, by the SDK, per the spec.
		traceOpts = append(traceOpts, otlptracehttp.WithEndpointURL(endpointURL))
	} else {
		traceOpts = append(traceOpts, otlptracehttp.WithEndpoint(hostPort), otlptracehttp.WithInsecure())
	}
	if len(headers) > 0 {
		traceOpts = append(traceOpts, otlptracehttp.WithHeaders(headers))
	}
	traceExporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(traceOpts...))
	if err != nil {
		log.Printf("otel: tracing is off, the trace exporter could not be created: %v", err)
		return noop
	}

	// Identify your application
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("onecamp-backend"),
			attribute.String("environment", "production"),
		),
	)
	if err != nil {
		log.Printf("otel: telemetry is off, the resource could not be described: %v", err)
		return noop
	}

	// --- Tracer Provider ---
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// --- Log Exporter ---
	logOpts := []otlploghttp.Option{}
	if endpointURL != "" {
		logOpts = append(logOpts, otlploghttp.WithEndpointURL(endpointURL))
	} else {
		logOpts = append(logOpts, otlploghttp.WithEndpoint(hostPort), otlploghttp.WithInsecure())
	}
	if len(headers) > 0 {
		logOpts = append(logOpts, otlploghttp.WithHeaders(headers))
	}
	logExporter, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		// Traces are already running, so shut them down rather than leaking a
		// provider that nothing will ever stop.
		log.Printf("otel: log export is off, the log exporter could not be created: %v", err)
		return func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				log.Printf("Error shutting down tracer provider: %v", err)
			}
		}
	}

	// --- Logger Provider ---
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	// Return a shutdown function
	return func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
		if err := lp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down logger provider: %v", err)
		}
	}
}
