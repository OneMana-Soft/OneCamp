package helpers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

// What isn't a failure isn't an ERROR. On the demo most error rows were one
// of two things that failed no one: a read called off because its client went
// away ("context canceled", once per layer it passed through), and a lookup
// that found nothing, which its caller answered with a 404.
func TestTheLogHelperKeepsErrorsForFailures(t *testing.T) {
	gone, leave := context.WithCancel(context.Background())
	leave()
	late, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-late.Done()

	for _, c := range []struct {
		name  string
		ctx   context.Context
		args  []interface{}
		level slog.Level
		attr  string
	}{
		{"the client went away", gone, []interface{}{context.Canceled}, slog.LevelDebug, "client_gone"},
		{"a lookup that found nothing", context.Background(), []interface{}{NotFound("failed to get dgraph user")}, slog.LevelDebug, "not_found"},
		{"found nothing, wrapped", context.Background(), []interface{}{fmt.Errorf("domain: %w", NotFound("failed to get dgraph project"))}, slog.LevelDebug, "not_found"},
		{"the server's own deadline", late, []interface{}{context.DeadlineExceeded}, slog.LevelError, ""},
		{"a store that didn't answer", context.Background(), []interface{}{errors.New("rpc error: code = Unavailable")}, slog.LevelError, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := withCapturedLogger(t)
			LogErrorWithContext(c.ctx, "domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v", c.args...)
			if len(h.records) != 1 {
				t.Fatalf("%d records, want 1", len(h.records))
			}
			if got := h.records[0].Level; got != c.level {
				t.Errorf("logged at %v, want %v", got, c.level)
			}
			if c.attr != "" {
				if v, ok := h.attr(c.attr); !ok || !v.Bool() {
					t.Errorf("no %s attribute saying why it isn't an error", c.attr)
				}
			}
		})
	}
}
