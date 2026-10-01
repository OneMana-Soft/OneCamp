package controllers

import (
	"net/http"
	"testing"
	"time"
)

func TestParseAuditExportRange(t *testing.T) {
	for _, c := range []struct {
		name      string
		query     string
		wantFrom  string // RFC3339, or empty
		wantTo    string // RFC3339, or empty
		wantError bool
	}{
		{
			name:     "no range",
			query:    "",
			wantFrom: "",
			wantTo:   "",
		},
		{
			name:     "calendar dates",
			query:    "from=2026-08-01&to=2026-08-19",
			wantFrom: "2026-08-01T00:00:00Z",
			// `to` date is normalised to the last nanosecond of the day.
			wantTo: "2026-08-19T23:59:59.999999999Z",
		},
		{
			name:     "full RFC3339 timestamps",
			query:    "from=2026-08-01T12:00:00Z&to=2026-08-19T12:00:00Z",
			wantFrom: "2026-08-01T12:00:00Z",
			wantTo:   "2026-08-19T12:00:00Z",
		},
		{
			name:      "from after to",
			query:     "from=2026-08-19&to=2026-08-01",
			wantError: true,
		},
		{
			name:      "invalid from",
			query:     "from=not-a-date",
			wantError: true,
		},
		{
			name:      "invalid to",
			query:     "to=also-not-a-date",
			wantError: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "/admin/audit-log/export?"+c.query, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			from, to, perr := parseAuditExportRange(req)
			if c.wantError {
				if perr == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if c.wantFrom == "" {
				if from != nil {
					t.Fatalf("expected nil from, got %v", from)
				}
			} else {
				if from == nil {
					t.Fatal("expected non-nil from")
				}
				if got := from.Format(time.RFC3339Nano); got != c.wantFrom {
					t.Fatalf("from = %s, want %s", got, c.wantFrom)
				}
			}
			if c.wantTo == "" {
				if to != nil {
					t.Fatalf("expected nil to, got %v", to)
				}
			} else {
				if to == nil {
					t.Fatal("expected non-nil to")
				}
				if got := to.Format(time.RFC3339Nano); got != c.wantTo {
					t.Fatalf("to = %s, want %s", got, c.wantTo)
				}
			}
		})
	}
}
