package helpers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// A read that found nothing is an answer, and logs nothing; a read that
// failed is logged, at error, under where, with its error. On the demo every
// doc deleted for good left two sweeps' reads logging "failed to get dgraph
// doc" at error, a minute apart, for a find they both skip.
func TestOnlyAReadThatFailedIsLogged(t *testing.T) {
	logs := withCapturedLogger(t)
	gone := NotFound("failed to get dgraph doc")
	ctx := context.Background()

	for _, quiet := range []error{nil, ErrNotFound, gone, fmt.Errorf("reading the doc: %w", gone)} {
		if LogReadFailure(ctx, quiet, "domain/GetCollabDocByUUID doc d1") {
			t.Errorf("%v was logged as a failed read", quiet)
		}
	}
	if len(logs.records) != 0 {
		t.Fatalf("a read that found nothing logged %d lines: %q", len(logs.records), logs.records[0].Message)
	}

	if !LogReadFailure(ctx, errors.New("context deadline exceeded"), "domain/GetCollabDocByUUID doc d1") {
		t.Fatal("a failed read wasn't logged")
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelError ||
		!strings.Contains(logs.records[0].Message, "domain/GetCollabDocByUUID doc d1: context deadline exceeded") {
		t.Fatalf("a failed read's line: %+v", logs.records)
	}
}
