package loggerInit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// InitLogger initializes the global structured logger and configures the helpers package
func InitLogger() {
	// Initialize Stdout JSON Handler
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// Initialize OTLP Handler (bridges slog to OpenTelemetry)
	// Note: This relies on global LoggerProvider being set in otelInit
	otelHandler := otelslog.NewHandler("onecamp-backend")

	// Create a TeeHandler that writes to both
	multiHandler := TeeHandler{
		Handlers: []slog.Handler{jsonHandler, otelHandler},
	}

	logger := slog.New(multiHandler)

	// Set as global default for slog
	slog.SetDefault(logger)

	// Configure helpers package globals
	helpers.Logger = logger
	helpers.MessageLogs = &helpers.Message{
		InfoLog:  slog.NewLogLogger(multiHandler, slog.LevelInfo),
		ErrorLog: slog.NewLogLogger(multiHandler, slog.LevelError),
	}
}

// TeeHandler broadcasts records to multiple handlers
type TeeHandler struct {
	Handlers []slog.Handler
}

func (t TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range t.Handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle sends r to every handler with each URL in its text cut before its
// query (helpers.WithoutURLQueries). A link a vendor signs, or a key or token
// in a query, is a working credential, and both sinks keep a line: otel_logs
// for 30 days, and the container's log. Only text is cut (a string, an error's
// message, either inside a group); a *url.URL, a Stringer or a struct reaches
// the sinks as it is, so a URL is logged as a string.
func (t TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	r = withoutURLQueries(r)
	var errs []error
	for _, h := range t.Handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("multiple handler errors: %v", errs)
	}
	return nil
}

// WithAttrs is t with attrs on every line, their URLs cut as Handle cuts a
// line's own.
func (t TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = slog.Attr{Key: a.Key, Value: valueWithoutURLQueries(a.Value)}
	}
	handlers := make([]slog.Handler, len(t.Handlers))
	for i, h := range t.Handlers {
		handlers[i] = h.WithAttrs(clean)
	}
	return TeeHandler{Handlers: handlers}
}

func (t TeeHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(t.Handlers))
	for i, h := range t.Handlers {
		handlers[i] = h.WithGroup(name)
	}
	return TeeHandler{Handlers: handlers}
}

// withoutURLQueries is r with helpers.WithoutURLQueries done to its message and
// to each attribute's text; r itself when none of them holds a URL, so a line
// without one costs a scan and no copy. The scan resolves an attribute that is
// a LogValuer, and each sink resolves it again, so a line holding one pays for
// one more LogValue.
func withoutURLQueries(r slog.Record) slog.Record {
	urls := strings.Contains(r.Message, "://")
	if !urls {
		r.Attrs(func(a slog.Attr) bool {
			urls = hasURL(a.Value)
			return !urls
		})
	}
	if !urls {
		return r
	}
	clean := slog.NewRecord(r.Time, r.Level, helpers.WithoutURLQueries(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(slog.Attr{Key: a.Key, Value: valueWithoutURLQueries(a.Value)})
		return true
	})
	return clean
}

// hasURL: v's text (a string, an error's message, or either in a group) holds
// a URL.
func hasURL(v slog.Value) bool {
	switch v = v.Resolve(); v.Kind() {
	case slog.KindString:
		return strings.Contains(v.String(), "://")
	case slog.KindGroup:
		for _, a := range v.Group() {
			if hasURL(a.Value) {
				return true
			}
		}
	case slog.KindAny:
		err, ok := v.Any().(error)
		return ok && strings.Contains(err.Error(), "://")
	}
	return false
}

// valueWithoutURLQueries is v with helpers.WithoutURLQueries done to its text.
// An error whose message holds a URL stays an error, so the OTel bridge still
// records it as the line's exception, with the cleaned message.
func valueWithoutURLQueries(v slog.Value) slog.Value {
	switch v = v.Resolve(); v.Kind() {
	case slog.KindString:
		return slog.StringValue(helpers.WithoutURLQueries(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]slog.Attr, len(group))
		for i, a := range group {
			clean[i] = slog.Attr{Key: a.Key, Value: valueWithoutURLQueries(a.Value)}
		}
		return slog.GroupValue(clean...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && strings.Contains(err.Error(), "://") {
			return slog.AnyValue(errors.New(helpers.WithoutURLQueries(err.Error())))
		}
	}
	return v
}
