package business

import (
	"context"
	"errors"
	"testing"
)

// A cancelled import stops before its next page: a `break` inside the select
// on ctx.Done() left only the select, so a cancelled GitHub import kept
// fetching pages (SA4011).
func TestACancelledImportFetchesNoMorePages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fetched := 0
	got := importPages(ctx, 10, func(page int) (bool, error) {
		fetched++
		if page == 2 {
			cancel()
		}
		return true, nil
	})
	if fetched != 2 || got != "context cancelled" {
		t.Fatalf("cancelled on page 2: fetched %d pages, ended %q; want 2 and \"context cancelled\"", fetched, got)
	}

	fetched = 0
	if got := importPages(context.Background(), 10, func(page int) (bool, error) {
		fetched++
		return page < 3, nil
	}); got != "" || fetched != 3 {
		t.Fatalf("a walk to its last page: fetched %d, ended %q", fetched, got)
	}
	if got := importPages(context.Background(), 10, func(int) (bool, error) {
		return true, errors.New("rate limited")
	}); got != "rate limited" {
		t.Fatalf("a failed page ends the walk as its error: %q", got)
	}
	fetched = 0
	importPages(context.Background(), 4, func(int) (bool, error) { fetched++; return true, nil })
	if fetched != 4 {
		t.Fatalf("at most max pages: fetched %d", fetched)
	}
}
