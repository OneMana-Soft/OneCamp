package models

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// fakeUpdates answers update-by-query runs from a script of responses, and
// records the body each run sent.
func fakeUpdates(t *testing.T, answers ...opensearchapi.UpdateByQueryResp) *[]string {
	t.Helper()
	sent := []string{}
	oldRun, oldWait := updateByQuery, conflictBackoff
	t.Cleanup(func() { updateByQuery, conflictBackoff = oldRun, oldWait })
	conflictBackoff = func(int) time.Duration { return 0 }
	updateByQuery = func(_ context.Context, req opensearchapi.UpdateByQueryReq) (*opensearchapi.UpdateByQueryResp, error) {
		b, _ := io.ReadAll(req.Body)
		sent = append(sent, string(b))
		if req.Params.Conflicts != "proceed" || req.Params.Refresh == nil || !*req.Params.Refresh {
			t.Errorf("a run without conflicts=proceed and refresh: %+v", req.Params)
		}
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

func TestAnUpdateByQueryStopsWaitingWhenItsCallerDoes(t *testing.T) {
	fakeUpdates(t, opensearchapi.UpdateByQueryResp{VersionConflicts: 1})
	conflictBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := updateByQuerySettled(ctx, []string{"comments"}, `{}`); err == nil {
		t.Fatal("a cancelled caller kept waiting to run again")
	}
}
