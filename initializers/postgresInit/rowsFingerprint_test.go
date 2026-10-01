package postgresInit

import (
	"context"
	"strings"
	"testing"
)

// The query is assembled from names, so the one thing that must never happen
// is a name that is not a name reaching it. Every call site passes constants;
// this is what keeps a future one honest.
func TestRowsFingerprintRefusesAnythingButAPlainTableName(t *testing.T) {
	for _, bad := range []string{
		"",
		"ai_settings; DROP TABLE users",
		"ai_settings)",
		"Ai_Settings",
		"ai settings",
		"1ai",
	} {
		if _, err := RowsFingerprint(context.Background(), bad); err == nil ||
			!strings.Contains(err.Error(), "not a plain table name") {
			t.Errorf("%q: expected a refusal naming the bad table, got %v", bad, err)
		}
	}
}

func TestRowsFingerprintNeedsAtLeastOneTable(t *testing.T) {
	if _, err := RowsFingerprint(context.Background()); err == nil {
		t.Fatal("a fingerprint over no tables would be a constant, and a constant never changes")
	}
}

func TestRowsFingerprintReportsAMissingDatabaseRatherThanPanicking(t *testing.T) {
	// The reconciler runs from boot; a nil connection here is a programming
	// error at the call site and must surface as an error, not a crash in a
	// goroutine nothing is watching.
	saved := DBConn
	DBConn = nil
	defer func() { DBConn = saved }()
	if _, err := RowsFingerprint(context.Background(), "ai_settings"); err == nil {
		t.Fatal("expected an error with no database connection")
	}
}
