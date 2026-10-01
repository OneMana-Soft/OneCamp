package loggerInit

import (
	"context"
	"fmt"
	"log/slog"
	"os"

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

func (t TeeHandler) Handle(ctx context.Context, r slog.Record) error {
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

func (t TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(t.Handlers))
	for i, h := range t.Handlers {
		handlers[i] = h.WithAttrs(attrs)
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
