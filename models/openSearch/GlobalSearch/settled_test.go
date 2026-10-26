package models

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// fakeUpdates answers update-by-query runs from a script of responses, and
// records the body each run sent. Each run must come right after a refresh of
// its own indices.
func fakeUpdates(t *testing.T, answers ...opensearchapi.UpdateByQueryResp) *[]string {
	t.Helper()
	sent := []string{}
	var refreshed []string
	oldRun, oldRefresh, oldWait := updateByQuery, refresh, conflictBackoff
	t.Cleanup(func() { updateByQuery, refresh, conflictBackoff = oldRun, oldRefresh, oldWait })
	conflictBackoff = func(int) time.Duration { return 0 }
	refresh = func(_ context.Context, indices ...string) error {
		refreshed = indices
		return nil
	}
	updateByQuery = func(_ context.Context, req opensearchapi.UpdateByQueryReq) (*opensearchapi.UpdateByQueryResp, error) {
		b, _ := io.ReadAll(req.Body)
		sent = append(sent, string(b))
		if req.Params.Conflicts != "proceed" || req.Params.Refresh == nil || !*req.Params.Refresh {
			t.Errorf("a run without conflicts=proceed and refresh: %+v", req.Params)
		}
		if !slices.Equal(refreshed, req.Indices) {
			t.Errorf("run %d over %v came after a refresh of %v; want its own indices refreshed first", len(sent), req.Indices, refreshed)
		}
		refreshed = nil
		i := len(sent) - 1
		if i >= len(answers) {
			i = len(answers) - 1
		}
		a := answers[i]
		return &a, nil
	}
	return &sent
}

func TestAnUpdateByQueryRunsAgainUntilNoEntryWasHeldByAnotherWrite(t *testing.T) {
	sent := fakeUpdates(t,
		opensearchapi.UpdateByQueryResp{Updated: 2, VersionConflicts: 1},
		opensearchapi.UpdateByQueryResp{Updated: 1},
	)
	updated, err := updateByQuerySettled(context.Background(), []string{"comments"}, `{"q": 1}`)
	if err != nil {
		t.Fatal(err)
	}
	if updated != 3 {
		t.Errorf("updated %d, want the 2 of the first run and the 1 held back", updated)
	}
	if len(*sent) != 2 || (*sent)[0] != `{"q": 1}` || (*sent)[1] != `{"q": 1}` {
		t.Errorf("runs sent %q; want the whole body twice", *sent)
	}
}

func TestAnUpdateByQueryStillHeldAfterTheLastRunIsAnError(t *testing.T) {
	sent := fakeUpdates(t, opensearchapi.UpdateByQueryResp{VersionConflicts: 2})
	_, err := updateByQuerySettled(context.Background(), []string{"comments"}, `{}`)
	if err == nil || !strings.Contains(err.Error(), "still not rewritten") {
		t.Fatalf("err = %v; want entries still held to be an error", err)
	}
	if len(*sent) != conflictAttempts {
		t.Errorf("%d runs, want %d", len(*sent), conflictAttempts)
	}
}

func TestAnUpdateByQueryWithFailuresIsAnErrorAtOnce(t *testing.T) {
	sent := fakeUpdates(t, opensearchapi.UpdateByQueryResp{
		Updated:  1,
		Failures: []json.RawMessage{json.RawMessage(`{"index": "comments", "id": "c1"}`)},
	})
	updated, err := updateByQuerySettled(context.Background(), []string{"comments"}, `{}`)
	if err == nil {
		t.Fatal("a run with failures answered no error")
	}
	if updated != 1 || len(*sent) != 1 {
		t.Errorf("updated %d after %d runs; want 1 after 1", updated, len(*sent))
	}
}

// An update-by-query matches only what the last refresh made visible, and a
// write (a bulk one, an import's) doesn't refresh: each run refreshes its own
// indices first (fakeUpdates checks it), so an entry written a moment before
// is matched too.
func TestEachRunOfAnUpdateByQueryRefreshesItsIndicesFirst(t *testing.T) {
	sent := fakeUpdates(t,
		opensearchapi.UpdateByQueryResp{VersionConflicts: 1},
		opensearchapi.UpdateByQueryResp{Updated: 1},
	)
	if _, err := updateByQuerySettled(context.Background(), []string{"comments", "attachments"}, `{}`); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 {
		t.Errorf("%d runs, want the held one run again", len(*sent))
	}
}

func TestARefreshThatFailsStopsTheUpdateByQueryBeforeItRuns(t *testing.T) {
	sent := fakeUpdates(t, opensearchapi.UpdateByQueryResp{Updated: 1})
	refresh = func(context.Context, ...string) error { return errors.New("search is down") }
	if _, err := updateByQuerySettled(context.Background(), []string{"comments"}, `{}`); err == nil || !strings.Contains(err.Error(), "search is down") {
		t.Fatalf("err = %v; want the refresh's error", err)
	}
	if len(*sent) != 0 {
		t.Errorf("%d runs after the refresh failed; want none", len(*sent))
	}
}

func TestAnUpdateByQueryStopsWaitingWhenItsCallerDoes(t *testing.T) {
	fakeUpdates(t, opensearchapi.UpdateByQueryResp{VersionConflicts: 1})
	conflictBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := updateByQuerySettled(ctx, []string{"comments"}, `{}`); err == nil {
		t.Fatal("a cancelled caller kept waiting to run again")
	}
}
