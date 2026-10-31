package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

func launch() querySpec.Spec {
	return querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("text", querySpec.OperatorContains, querySpec.Value{Text: "launch"})}
}

// A restricted task the asker may not see, with its comments and files, is
// left out inside the index's query: so it is in no page, and in no count
// (the counts are an aggregation over that same query).
func TestARestrictedTaskIsInNoPageAndNoCount(t *testing.T) {
	allReady(t)
	const secret = "7a1c9a52-3e1b-4c55-9d2f-0c6f3c1a0002"
	swap(t, &taskread.Decide, func(context.Context, string) (map[string]bool, error) {
		return map[string]bool{secret: true}, nil
	})
	var body struct {
		Query struct {
			Bool struct {
				Must    []json.RawMessage                `json:"must"`
				MustNot []map[string]map[string][]string `json:"must_not"`
			} `json:"bool"`
		} `json:"query"`
		Aggs json.RawMessage `json:"aggs"`
	}
	swap(t, &searchIndex, func(_ context.Context, _ []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return opensearchModels.SearchAnswer{}, nil
	})
	if _, err := Search(context.Background(), sample, launch(), Page{Kinds: []string{KindTask}}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]map[string][]string{
		{"terms": {"task_id": {secret}}},
		{"terms": {"comment_task_id": {secret}}},
		{"terms": {"attachment_task_id": {secret}}},
	}
	if !reflect.DeepEqual(body.Query.Bool.MustNot, want) || len(body.Query.Bool.Must) != 1 {
		t.Errorf("the query leaves out %v around %d queries; want %v around the search's own", body.Query.Bool.MustNot, len(body.Query.Bool.Must), want)
	}
	if len(body.Aggs) == 0 {
		t.Error("the counts went missing from the body")
	}
}

// A search whose restricted tasks can't be worked out fails: it never runs
// as though nothing were restricted.
func TestASearchFailsWhenItsRestrictedTasksCantBeWorkedOut(t *testing.T) {
	allReady(t)
	swap(t, &taskread.Decide, func(context.Context, string) (map[string]bool, error) {
		return nil, errors.New("graph down")
	})
	asked := false
	swap(t, &searchIndex, func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		asked = true
		return opensearchModels.SearchAnswer{}, nil
	})
	if _, err := Search(context.Background(), sample, launch(), Page{}); err == nil || asked {
		t.Errorf("err %v, index asked %v; want an error and no search", err, asked)
	}
}

// A key typed into the engine's search finds its task first (the task keys'
// exact step), every task hit carries its key, and a restricted task the
// asker may not see is not found by its key.
func TestAKeyFindsItsTaskThroughTheEngine(t *testing.T) {
	allReady(t)
	key, project := uuid.MustParse(liveTask), uuid.NewString()
	withChecks(t, map[string]liveness.Checker{
		"task": func(_ context.Context, ids []string) (map[string]bool, error) {
			out := map[string]bool{}
			for _, id := range ids {
				out[id] = true
			}
			return out, nil
		},
	})
	swap(t, &resolveKey, func(_ context.Context, raw string) (uuid.UUID, error) {
		if k, n, ok := taskKeys.Parse(raw); ok && k == "Q4" && n == 12 {
			return key, nil
		}
		return uuid.Nil, taskKeys.ErrNoTask
	})
	swap(t, &readTask, func(_ context.Context, id string, _ string) (*dgraphStruct.DgraphTask, error) {
		return &dgraphStruct.DgraphTask{Uuid: id, Name: "Pricing page copy", Project: &dgraphStruct.DgraphProject{Uuid: project, Name: "Q4 launch"}}, nil
	})
	swap(t, &keysOf, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		out := map[uuid.UUID]string{}
		for _, id := range ids {
			out[id] = "K-" + id.String()[:4]
		}
		return out, nil
	})
	other := "7a1c9a52-3e1b-4c55-9d2f-0c6f3c1a0001"
	swap(t, &searchIndex, func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		return opensearchModels.SearchAnswer{Hits: []*openSearchStruct.GlobalSearchOpenSearchResp{task(other)}, ByIndex: map[string]int{openSearchStruct.TASK_INDEX: 1}, Total: 1}, nil
	})
	reach := Reach{UserUUID: "U", Projects: []string{project}}
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("text", querySpec.OperatorContains, querySpec.Value{Text: "q4-12"})}

	p, err := Search(context.Background(), reach, spec, Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 2 || p.Page[0].Task.Uuid != liveTask || p.Total != 2 {
		t.Fatalf("page %+v, total %d; want the key's task first, then the index's, 2 in all", p.Page, p.Total)
	}
	for _, h := range p.Page {
		if h.Task.TaskKey == "" {
			t.Errorf("task %s has no key", h.Task.Uuid)
		}
	}

	swap(t, &taskread.Decide, func(context.Context, string) (map[string]bool, error) {
		return map[string]bool{liveTask: true}, nil
	})
	if p, err = Search(context.Background(), reach, spec, Page{}); err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 1 || p.Page[0].Task.Uuid != other || p.Total != 1 {
		t.Errorf("a restricted task found by its key: page %+v, total %d", p.Page, p.Total)
	}
}

// A hit the live check drops (an index entry that says the asker may read a
// doc or board they may not) comes off its kind's count on that page, and
// has that one entry written again from the doc or board, in the background
// and at most once a while however often it is searched for.
func TestADroppedHitComesOffItsCountAndHasItsEntryRepairedOnce(t *testing.T) {
	allReady(t)
	// Fresh ids: the once-a-while limit outlives a test.
	hidden, open, board := uuid.NewString(), uuid.NewString(), uuid.NewString()
	withChecks(t, nil)
	withReadable(t, func(_ context.Context, docs []string, _ string) (map[string]bool, error) {
		return map[string]bool{open: true}, nil
	})
	withReadableBoards(t, func(context.Context, []string, string) (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	var repaired []string
	swap(t, &repairEntry, func(kind, id string) { repaired = append(repaired, kind+":"+id) })
	swap(t, &goRepairs, func(repair func()) { repair() })
	swap(t, &searchIndex, func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		return opensearchModels.SearchAnswer{
			Hits:    []*openSearchStruct.GlobalSearchOpenSearchResp{docHit(hidden), docHit(open), boardHit(board)},
			ByIndex: map[string]int{openSearchStruct.DOC_INDEX: 30, openSearchStruct.BOARD_INDEX: 4},
			Total:   34,
		}, nil
	})
	reach := Reach{UserUUID: "U"}
	for range 3 {
		p, err := Search(context.Background(), reach, launch(), Page{Size: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Page) != 1 || p.Totals[KindDoc] != 29 || p.Totals[KindBoard] != 3 || p.Total != 32 {
			t.Fatalf("page %d, totals %v, total %d; want 1 shown, docs 29, boards 3, 32 in all", len(p.Page), p.Totals, p.Total)
		}
	}
	if want := []string{"doc:" + hidden, "board:" + board}; !reflect.DeepEqual(repaired, want) {
		t.Errorf("repaired %v; want %v, each once", repaired, want)
	}
}

// A cursor belongs to the search that wrote it: the same asker, query, kinds
// on show and order. Sent with any other, or with its place moved, it is
// refused before the index is asked.
func TestACursorIsRefusedOutsideItsOwnSearch(t *testing.T) {
	allReady(t)
	asked := 0
	swap(t, &searchIndex, func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		asked++
		hits := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 2)
		for i := range hits {
			hits[i] = docHit(uuid.NewString())
		}
		return opensearchModels.SearchAnswer{Hits: hits, ByIndex: map[string]int{openSearchStruct.DOC_INDEX: 9}, Total: 9}, nil
	})
	withChecks(t, nil)
	withReadable(t, func(_ context.Context, docs []string, _ string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, d := range docs {
			out[d] = true
		}
		return out, nil
	})
	page := Page{Size: 2, Kinds: []string{KindDoc}, Sort: SortNewest}
	first, err := Search(context.Background(), sample, launch(), page)
	if err != nil || first.Next == "" {
		t.Fatalf("first page: next %q, %v", first.Next, err)
	}
	next := page
	next.After = first.Next
	if _, err := Search(context.Background(), sample, launch(), next); err != nil {
		t.Fatalf("its own next page: %v", err)
	}

	other := launch()
	other.Filter = cond("text", querySpec.OperatorContains, querySpec.Value{Text: "pricing"})
	otherAsker := sample
	otherAsker.UserUUID = "V"
	otherKinds, otherSort := next, next
	otherKinds.Kinds = []string{KindTask}
	otherSort.Sort = SortOldest
	moved, err := json.Marshal(map[string]any{"o": 4, "q": cursorBinding(sample, launch(), page)[:15] + "0"})
	if err != nil {
		t.Fatal(err)
	}
	movedPage := next
	movedPage.After = base64.RawURLEncoding.EncodeToString(moved)
	asked = 0
	for what, try := range map[string]func() error{
		"another query":    func() error { _, err := Search(context.Background(), sample, other, next); return err },
		"another asker":    func() error { _, err := Search(context.Background(), otherAsker, launch(), next); return err },
		"other kinds":      func() error { _, err := Search(context.Background(), sample, launch(), otherKinds); return err },
		"another order":    func() error { _, err := Search(context.Background(), sample, launch(), otherSort); return err },
		"a forged binding": func() error { _, err := Search(context.Background(), sample, launch(), movedPage); return err },
	} {
		if err := try(); !errors.Is(err, ErrBadSearchCursor) {
			t.Errorf("%s: %v; want ErrBadSearchCursor", what, err)
		}
	}
	if asked != 0 {
		t.Errorf("the index was asked %d times for refused cursors", asked)
	}
}

// The exact step leaves what the text names exactly out of the index's pages
// by an ids clause in its source's must_not. It wrote that clause in place of
// the must_not, so whatever the source's query left out itself (a deleted
// entry, a project's tasks) would have come back with it. It adds to it now.
func TestTheExactStepKeepsWhatASourceLeavesOut(t *testing.T) {
	q := map[string]any{"bool": map[string]any{
		"filter":   []map[string]any{term("_index", openSearchStruct.TASK_INDEX)},
		"must_not": []map[string]any{{"exists": map[string]any{"field": "task_project_id"}}},
	}}
	leaveOut(q, liveTask)
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Bool struct {
			MustNot []map[string]any `json:"must_not"`
		} `json:"bool"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"exists": map[string]any{"field": "task_project_id"}},
		{"ids": map[string]any{"values": []any{liveTask}}},
	}
	if !reflect.DeepEqual(got.Bool.MustNot, want) {
		t.Errorf("the source's must_not after the exact step: %v; want what it left out and the exact entry", got.Bool.MustNot)
	}
}
