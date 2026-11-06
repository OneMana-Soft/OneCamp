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

// ForgetWhere removes, for good, every entry whose field holds one of values:
// what a purge past the trash leaves in search (a board's entry, its
// comments', its AI text). It waits for the delete, skips indices that don't
// exist (no AI, no AI index), and runs again while entries it matched were
// being written by something else, so none is left behind. Each run comes
// right after a refresh of its own indices: a delete-by-query sees only what
// the last refresh showed (b204).
func TestForgetWhereRemovesEveryMatchingEntry(t *testing.T) {
	oldRun, oldRefresh, oldWait := deleteByQuery, refreshPresent, conflictBackoff
	t.Cleanup(func() { deleteByQuery, refreshPresent, conflictBackoff = oldRun, oldRefresh, oldWait })
	conflictBackoff = func(int) time.Duration { return 0 }
	var refreshed []string
	refreshPresent = func(_ context.Context, indices ...string) error {
		refreshed = indices
		return nil
	}
	var bodies []string
	runs := 0
	deleteByQuery = func(_ context.Context, req opensearchapi.DocumentDeleteByQueryReq) (*opensearchapi.DocumentDeleteByQueryResp, error) {
		runs++
		if !slices.Equal(refreshed, req.Indices) {
			t.Errorf("run %d over %v came after a refresh of %v; want its own indices refreshed first", runs, req.Indices, refreshed)
		}
		refreshed = nil
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
		p := req.Params
		if p.Refresh == nil || !*p.Refresh || p.WaitForCompletion == nil || !*p.WaitForCompletion || p.Conflicts != "proceed" ||
			p.IgnoreUnavailable == nil || !*p.IgnoreUnavailable || p.AllowNoIndices == nil || !*p.AllowNoIndices {
			t.Errorf("params = %+v; want refreshed, waited for, conflicts proceed, missing indices skipped", p)
		}
		if strings.Join(req.Indices, ",") != "comments" {
			t.Errorf("indices = %v", req.Indices)
		}
		resp := &opensearchapi.DocumentDeleteByQueryResp{Deleted: 2}
		if runs == 1 {
			resp.VersionConflicts = 1
		}
		return resp, nil
	}
	n, err := ForgetWhere(context.Background(), []string{"comments"}, []string{"comment_board_id"}, []string{"b1", `b"2`})
	if err != nil || n != 4 || runs != 2 {
		t.Fatalf("ForgetWhere = %d, %v after %d runs; want 4, nil after 2 (a conflict runs it again)", n, err, runs)
	}
	var q struct {
		Query struct {
			Bool struct {
				Should []struct {
					Terms map[string][]string `json:"terms"`
				} `json:"should"`
			} `json:"bool"`
		} `json:"query"`
	}
	err = json.Unmarshal([]byte(bodies[0]), &q)
	if should := q.Query.Bool.Should; err != nil || len(should) != 2 || strings.Join(should[0].Terms["comment_board_id"], "|") != `b1|b"2` ||
		strings.Join(should[1].Terms["comment_board_id.keyword"], "|") != `b1|b"2` {
		t.Errorf("body = %s (%v); want terms on comment_board_id and on its keyword sub-field (a text-mapped id never matches whole), values escaped", bodies[0], err)
	}

	runs = 0
	if n, err := ForgetWhere(context.Background(), []string{"comments"}, []string{"comment_board_id"}, nil); n != 0 || err != nil || runs != 0 {
		t.Errorf("no values: %d, %v after %d runs; want nothing asked", n, err, runs)
	}

	// Several fields, one delete: an entry goes when any of them names a value (a board's own entry by _id, its
	// comments' by comment_board_id), each field as itself and as its keyword sub-field.
	bodies = nil
	if _, err := ForgetWhere(context.Background(), []string{"comments"}, []string{"_id", "comment_board_id"}, []string{"b1"}); err != nil || runs != 2 || bodies[0] != bodies[1] {
		t.Fatalf("two fields: %v after %d runs; want one delete, asked again whole after its conflict", err, runs)
	}
	q.Query.Bool.Should = nil
	err = json.Unmarshal([]byte(bodies[0]), &q)
	var terms []string
	for _, c := range q.Query.Bool.Should {
		for f, v := range c.Terms {
			terms = append(terms, f+"="+strings.Join(v, "|"))
		}
	}
	if want := "_id=b1 _id.keyword=b1 comment_board_id=b1 comment_board_id.keyword=b1"; err != nil || strings.Join(terms, " ") != want {
		t.Errorf("two fields' body = %s (%v); want terms %s", bodies[0], err, want)
	}
	if n, err := ForgetWhere(context.Background(), []string{"comments"}, nil, []string{"b1"}); n != 0 || err != nil || runs != 2 {
		t.Errorf("no fields: %d, %v after %d runs; want nothing asked", n, err, runs)
	}
}

// A purge whose refresh fails deletes nothing and says why: a delete after a
// failed refresh could miss what the refresh would have shown, and answer done.
func TestAForgetWhoseRefreshFailsStopsBeforeItsDelete(t *testing.T) {
	oldRun, oldRefresh := deleteByQuery, refreshPresent
	t.Cleanup(func() { deleteByQuery, refreshPresent = oldRun, oldRefresh })
	refreshPresent = func(context.Context, ...string) error { return errors.New("search is down") }
	runs := 0
	deleteByQuery = func(context.Context, opensearchapi.DocumentDeleteByQueryReq) (*opensearchapi.DocumentDeleteByQueryResp, error) {
		runs++
		return &opensearchapi.DocumentDeleteByQueryResp{Deleted: 1}, nil
	}
	if n, err := ForgetWhere(context.Background(), []string{"comments"}, []string{"comment_board_id"}, []string{"b1"}); err == nil || !strings.Contains(err.Error(), "search is down") || n != 0 {
		t.Fatalf("ForgetWhere = %d, %v; want the refresh's error, nothing removed", n, err)
	}
	if runs != 0 {
		t.Errorf("%d deletes after the refresh failed; want none", runs)
	}
}
