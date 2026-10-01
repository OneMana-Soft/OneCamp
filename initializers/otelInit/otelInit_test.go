package otelInit

// How the collector address is read.
//
// This is worth pinning because the failure it guards against is invisible. A
// batch exporter reports a bad endpoint to the OTel error handler, nothing
// installs one, and the operator sees no error and no telemetry — the two
// things that look identical from outside are "the collector is quiet" and
// "every export has been failing since you configured it".

import (
	"testing"
)

func TestCollectorEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		wantURL  string
		wantHost string
	}{
		{
			// The OTLP specification's own spelling, and the one an operator
			// copying from any OpenTelemetry documentation will use.
			name: "a url is passed through as a url", env: "http://otel-collector:4318",
			wantURL: "http://otel-collector:4318",
		},
		{
			// The case that matters most: https must stay https. Handing this to
			// WithEndpoint alongside WithInsecure sent telemetry, including the
			// governance record, over plaintext to a host expecting TLS.
			name: "https keeps its scheme", env: "https://otel.example.com:4318",
			wantURL: "https://otel.example.com:4318",
		},
		{
			// This repository's own compose files use the bare form, so it has
			// to keep working for every install that already has one.
			name: "bare host and port still works", env: "otel-collector:4318",
			wantHost: "otel-collector:4318",
		},
		{name: "unset falls back to the local collector", env: "", wantHost: "localhost:4318"},
		{name: "whitespace is not configuration", env: "   ", wantHost: "localhost:4318"},
		{name: "surrounding whitespace is trimmed", env: "  https://c:4318 ", wantURL: "https://c:4318"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.env)
			gotURL, gotHost := collectorEndpoint()
			if gotURL != c.wantURL || gotHost != c.wantHost {
				t.Fatalf("collectorEndpoint() = (url=%q, host=%q), want (url=%q, host=%q)",
					gotURL, gotHost, c.wantURL, c.wantHost)
			}
			// Exactly one, always. Both set would apply two conflicting options
			// to the same exporter and the last one would quietly win.
			if (gotURL == "") == (gotHost == "") {
				t.Fatalf("expected exactly one of url/host to be set, got url=%q host=%q", gotURL, gotHost)
			}
		})
	}
}

func TestExportHeadersOmittedWhenThereIsNoCredential(t *testing.T) {
	for _, env := range []string{"", "   "} {
		t.Setenv("HYPERDX_INGESTION_API_KEY", env)
		if h := exportHeaders(); h != nil {
			t.Errorf("with key %q the headers were %v; an empty authorization header is not the same as none, and a collector that checks it rejects the batch", env, h)
		}
	}

	t.Setenv("HYPERDX_INGESTION_API_KEY", "sekret")
	if got := exportHeaders()["authorization"]; got != "sekret" {
		t.Errorf("authorization = %q, want the configured key", got)
	}
}
