package router

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A handler that panics answers what the app can show, and the request's span
// says where it happened. Behind chi's Recoverer alone the app got an empty
// 500, which it could only call "The server hit a problem", and the span ended
// with no route and no status: on the demo, four panics in a month left
// nothing in otel_traces or otel_logs saying which endpoint it was.
func TestAPanicIsAnsweredAndTracedUnderItsRoute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	var logged bytes.Buffer
	previousLogger := helpers.Logger
	helpers.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
	t.Cleanup(func() { helpers.Logger = previousLogger })

	const app = "https://app.acme.test"
	r := chi.NewRouter()
	useBaseMiddleware(r, []string{app})
	r.Get("/task/info/{task_uuid}", func(http.ResponseWriter, *http.Request) {
		panic("runtime error: invalid memory address or nil pointer dereference")
	})

	req := httptest.NewRequest(http.MethodGet, "/task/info/7c9e6679-7425-40de-944b-e07fc1f90ae7", nil)
	req.Header.Set("Origin", app)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != app {
		t.Errorf("Access-Control-Allow-Origin %q, want %q: without it the browser hides the answer", got, app)
	}
	var body struct {
		Msg any `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q isn't JSON: %v", rec.Body.String(), err)
	}
	if msg, ok := body.Msg.(string); !ok || strings.TrimSpace(msg) == "" {
		t.Errorf("msg %v, want a sentence the app shows in place of \"The server hit a problem\"", body.Msg)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans ended, want the request's one", len(spans))
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs["http.route"].AsString(); got != "/task/info/{task_uuid}" {
		t.Errorf("span route %q, want /task/info/{task_uuid}", got)
	}
	if got := attrs["http.status_code"].AsInt64(); got != http.StatusInternalServerError {
		t.Errorf("span status code %d, want 500", got)
	}

	// The log line otel_logs keeps says it all on its own: the route, the
	// status, the panic, the stack, and the trace it belongs to.
	var line struct {
		Level   string `json:"level"`
		Msg     string `json:"msg"`
		TraceID string `json:"trace_id"`
	}
	var found bool
	for _, raw := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if bytes.Contains(raw, []byte("RecoverPanic")) {
			if err := json.Unmarshal(raw, &line); err != nil {
				t.Fatalf("log line %q isn't JSON: %v", raw, err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no RecoverPanic line was logged; the log had:\n%s", logged.String())
	}
	for _, want := range []string{"GET /task/info/{task_uuid}", "answered 500", "nil pointer dereference", "panicRecovery_test.go"} {
		if !strings.Contains(line.Msg, want) {
			t.Errorf("log line lacks %q:\n%s", want, line.Msg)
		}
	}
	if line.Level != "ERROR" {
		t.Errorf("log level %q, want ERROR", line.Level)
	}
	if want := spans[0].SpanContext().TraceID().String(); line.TraceID != want {
		t.Errorf("log trace_id %q, want the request's %q", line.TraceID, want)
	}
}

// net/http aborts a response on purpose with http.ErrAbortHandler; that is not
// a failure to answer, and it must reach net/http as it was raised.
func TestAnAbortedResponseIsNotTurnedIntoA500(t *testing.T) {
	r := chi.NewRouter()
	useBaseMiddleware(r, nil)
	r.Get("/stream", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler passed on", rec)
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
	t.Error("the abort was swallowed")
}
