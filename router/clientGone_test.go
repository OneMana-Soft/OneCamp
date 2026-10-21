package router

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A request whose client went away (the task panel closed, the next task
// opened) is not a server failure. Its reads were called off, and the handler
// answered 503 to no one; on the demo every task-panel 5xx in a month was one
// of these, logged at ERROR by each layer the cancelled read passed through
// and counted as a 5xx in the traces beside the real ones.
func TestARequestItsClientLeftIsNeitherA5xxNorAnError(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	var logged bytes.Buffer
	previousLogger := helpers.Logger
	helpers.Logger = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { helpers.Logger = previousLogger })

	ctx, leave := context.WithCancel(context.Background())
	r := chi.NewRouter()
	useBaseMiddleware(r, nil)
	r.Get("/task/recurrence/{task_uuid}", func(w http.ResponseWriter, r *http.Request) {
		leave() // the browser closed the panel while the read was in flight
		helpers.LogErrorWithContext(r.Context(), "models/GetDgraphTaskInfoByUUID failed to get task err: %+v", r.Context().Err())
		helpers.WriteRetryLater(w, "Couldn't read the task.")
	})
	req := httptest.NewRequest(http.MethodGet, "/task/recurrence/7c9e6679-7425-40de-944b-e07fc1f90ae7", nil).WithContext(ctx)
	r.ServeHTTP(httptest.NewRecorder(), req)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans ended, want the request's one", len(spans))
	}
	var status int64
	var gone bool
	for _, kv := range spans[0].Attributes() {
		switch kv.Key {
		case attribute.Key("http.status_code"):
			status = kv.Value.AsInt64()
		case attribute.Key("http.client_gone"):
			gone = kv.Value.AsBool()
		}
	}
	if status != 499 {
		t.Errorf("span status code %d, want 499: the client was gone before it was answered", status)
	}
	if !gone {
		t.Error("the span doesn't say the client went away (http.client_gone)")
	}
	if spans[0].Status().Code == codes.Error {
		t.Errorf("span marked as an error (%q); no one was failed", spans[0].Status().Description)
	}

	var line struct {
		Level      string `json:"level"`
		ClientGone bool   `json:"client_gone"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &line); err != nil {
		t.Fatalf("log %q isn't one JSON line: %v", logged.String(), err)
	}
	if line.Level != "DEBUG" || !line.ClientGone {
		t.Errorf("logged at %s (client_gone=%v), want DEBUG with client_gone", line.Level, line.ClientGone)
	}
}

// The client still waiting gets the answer as written, and a real failure is
// still a 5xx and an ERROR.
func TestAFailureTheClientWaitsForIsStillA5xx(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	var logged bytes.Buffer
	previousLogger := helpers.Logger
	helpers.Logger = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { helpers.Logger = previousLogger })

	r := chi.NewRouter()
	useBaseMiddleware(r, nil)
	r.Get("/task/recurrence/{task_uuid}", func(w http.ResponseWriter, r *http.Request) {
		helpers.LogErrorWithContext(r.Context(), "models/GetDgraphTaskInfoByUUID failed to get task err: %+v", http.ErrHandlerTimeout)
		helpers.WriteRetryLater(w, "Couldn't read the task.")
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/task/recurrence/7c9e6679-7425-40de-944b-e07fc1f90ae7", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d, want the handler's 503", rec.Code)
	}
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans ended, want the request's one", len(ended))
	}
	if got := ended[0].Status().Code; got != codes.Error {
		t.Errorf("span status %v, want Error for a 503 someone received", got)
	}
	var line struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &line); err != nil || line.Level != "ERROR" {
		t.Errorf("logged %q, want one ERROR line", logged.String())
	}
}
