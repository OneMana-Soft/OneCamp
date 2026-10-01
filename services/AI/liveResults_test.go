package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
)

const (
	liveDoc  = "8611f10f-70f7-462e-b966-ab5c89423881"
	goneDoc  = "f2b3b6ff-0000-4000-8000-000000000001"
	trashDoc = "f2b3b6ff-0000-4000-8000-000000000002"
	livePost = "c3a99e21-0000-4000-8000-000000000003"
)

// withCheckers swaps the liveness checks and the forget seam for one test.
func withCheckers(t *testing.T, checkers map[string]liveness.Checker) *[]SimilarResult {
	t.Helper()
	oldCheckers, oldForget := liveCheckers, forgetStaleFn
	var forgotten []SimilarResult
	liveCheckers = checkers
	forgetStaleFn = func(stale []SimilarResult) { forgotten = append(forgotten, stale...) }
	t.Cleanup(func() { liveCheckers, forgetStaleFn = oldCheckers, oldForget })
	return &forgotten
}

func fixed(states map[string]bool) liveness.Checker {
	return func(context.Context, []string) (map[string]bool, error) { return states, nil }
}

func uuids(results []SimilarResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.ContentUUID
	}
	return out
}

// The demo cited the handbook four times and three of the links opened
// nothing: the docs were gone and their entries were not.
func TestAnAnswerNeverCitesContentThatIsGone(t *testing.T) {
	forgotten := withCheckers(t, map[string]liveness.Checker{
		"doc": fixed(map[string]bool{liveDoc: true, trashDoc: false}),
	})
	in := []SimilarResult{
		{ContentType: "doc", ContentUUID: goneDoc},
		{ContentType: "doc", ContentUUID: liveDoc},
		{ContentType: "doc", ContentUUID: trashDoc},
	}
	kept := KeepLive(context.Background(), in)
	if got := uuids(kept); len(got) != 1 || got[0] != liveDoc {
		t.Fatalf("kept %v, want only the live doc", got)
	}
	// Gone: its entry is deleted. Soft-deleted: hidden, but an admin can
	// restore it, so its entry stays.
	if got := uuids(*forgotten); len(got) != 1 || got[0] != goneDoc {
		t.Fatalf("forgot %v, want only the doc that no longer exists", got)
	}
}

func TestAFailedCheckKeepsTheResults(t *testing.T) {
	forgotten := withCheckers(t, map[string]liveness.Checker{
		"doc": func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("graph down") },
	})
	in := []SimilarResult{{ContentType: "doc", ContentUUID: goneDoc}}
	if got := KeepLive(context.Background(), in); len(got) != 1 {
		t.Fatalf("a check that could not run dropped results: %v", uuids(got))
	}
	if len(*forgotten) != 0 {
		t.Fatalf("a check that could not run deleted entries: %v", uuids(*forgotten))
	}
}

func TestTypesWithoutACheckAndOddIdsPassThrough(t *testing.T) {
	withCheckers(t, map[string]liveness.Checker{"post": fixed(map[string]bool{livePost: true})})
	in := []SimilarResult{
		{ContentType: "memory", ContentUUID: goneDoc},
		{ContentType: "post", ContentUUID: "not-a-uuid"},
		{ContentType: "post", ContentUUID: strings.ToUpper(livePost)},
	}
	if got := KeepLive(context.Background(), in); len(got) != 3 {
		t.Fatalf("kept %v, want all three", uuids(got))
	}
}
