package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/models/taskread"
)

// Who a run is for, and the one search filter it changes.

func TestARunIsForItsAskerOnlyWhenTheyAreNotTheSponsor(t *testing.T) {
	ctx := context.Background()
	if _, _, ok := RunRequester(ctx); ok {
		t.Fatal("a context nobody marked must read as a run for the sponsor alone")
	}

	asked := WithRunRequester(ctx, " asker ", "sponsor")
	requester, sponsor, ok := RunRequester(asked)
	if !ok || requester != "asker" || sponsor != "sponsor" {
		t.Fatalf("someone else asked: got (%q, %q, %v)", requester, sponsor, ok)
	}

	// The sponsor asking for their own agent's run is a run for the sponsor.
	if _, _, ok := RunRequester(WithRunRequester(ctx, "SPONSOR", "sponsor")); ok {
		t.Error("a run the sponsor asked for must act for the sponsor alone")
	}

	// A person asked and could not be identified: still a run for someone else,
	// with nobody to check against, which every check refuses.
	if requester, _, ok := RunRequester(WithRunRequester(ctx, "", "sponsor")); !ok || requester != "" {
		t.Errorf("an unidentified asker must not read as the sponsor: got (%q, %v)", requester, ok)
	}

	// A schedule launched from a context that carried someone's request is not
	// theirs.
	if _, _, ok := RunRequester(WithoutRunRequester(asked)); ok {
		t.Error("a run nobody asked for must clear a requester inherited from its context")
	}
	if who, asked := RunAskedBy(WithoutRunRequester(asked)); asked || who != "" {
		t.Errorf("RunAskedBy after clearing = (%q, %v)", who, asked)
	}
}

func TestASearchForSomeoneElseMustPassBothPeoplesFilters(t *testing.T) {
	defer func(prev func(context.Context, string) (ReaderScope, error)) { readerScopeResolver = prev }(readerScopeResolver)
	var resolved string
	RegisterReaderScopeResolver(func(_ context.Context, userUUID string) (ReaderScope, error) {
		resolved = userUUID
		return ReaderScope{Channels: []string{"shared-channel"}, GrpIDs: []string{"their-dm"}, Projects: []string{"their-project"}}, nil
	})

	base := buildPermissionFilter("sponsor", []string{"shared-channel", "private-channel"}, nil, nil)

	// A run for the sponsor is exactly the filter it always was.
	got, readers, err := runPermissionFilter(context.Background(), "sponsor", []string{"shared-channel", "private-channel"}, nil, nil)
	if err != nil || got != base {
		t.Fatalf("a run for the sponsor changed its filter (err %v)", err)
	}
	if len(readers) != 1 || readers[0].user != "sponsor" {
		t.Fatalf("a run for the sponsor reads for %+v, want the sponsor alone", readers)
	}
	if resolved != "" {
		t.Fatal("a run for the sponsor resolved somebody's scope it does not need")
	}

	ctx := WithRunRequester(context.Background(), "asker", "sponsor")
	got, readers, err = runPermissionFilter(ctx, "sponsor", []string{"shared-channel", "private-channel"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The task check after the search (keepAssigned) holds the asker to
	// their own projects, not the sponsor's.
	if len(readers) != 2 || readers[0].user != "sponsor" || readers[1].user != "asker" ||
		readers[0].covers("their-project") || !readers[1].covers("their-project") {
		t.Fatalf("a run for the asker reads for %+v, want the sponsor and the asker with their own projects", readers)
	}
	if resolved != "asker" {
		t.Fatalf("the scope resolved was %q's, want the asker's", resolved)
	}
	var parsed struct {
		Bool struct {
			Must []json.RawMessage `json:"must"`
		} `json:"bool"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("the combined filter is not JSON: %v\n%s", err, got)
	}
	if len(parsed.Bool.Must) != 2 {
		t.Fatalf("want the sponsor's filter AND the asker's, got %d clauses", len(parsed.Bool.Must))
	}
	theirs := buildPermissionFilter("asker", []string{"shared-channel"}, []string{"their-project"}, []string{"their-dm"})
	if compact(string(parsed.Bool.Must[0])) != compact(base) || compact(string(parsed.Bool.Must[1])) != compact(theirs) {
		t.Errorf("the two halves are not the two people's own filters:\n%s", got)
	}
}

func TestASearchForSomeoneUnresolvableDoesNotRun(t *testing.T) {
	defer func(prev func(context.Context, string) (ReaderScope, error)) { readerScopeResolver = prev }(readerScopeResolver)
	ctx := WithRunRequester(context.Background(), "asker", "sponsor")

	readerScopeResolver = nil
	if _, _, err := runPermissionFilter(ctx, "sponsor", nil, nil, nil); err == nil {
		t.Error("with no way to resolve the asker, the search must refuse rather than run as the sponsor")
	}

	RegisterReaderScopeResolver(func(context.Context, string) (ReaderScope, error) {
		return ReaderScope{}, errors.New("graph down")
	})
	if _, _, err := runPermissionFilter(ctx, "sponsor", nil, nil, nil); err == nil {
		t.Error("a failed lookup must refuse the search")
	}

	unidentified := WithRunRequester(context.Background(), "", "sponsor")
	if _, _, err := runPermissionFilter(unidentified, "sponsor", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "could not be identified") {
		t.Errorf("an unidentified asker must refuse the search, got %v", err)
	}
}

// compact strips whitespace so two renderings of the same JSON compare equal.
func compact(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// A run remembers what it resolved, but not a failure, and nothing is kept
// outside a run someone asked for.
func TestARunRemembersWhatItResolved(t *testing.T) {
	calls := 0
	compute := func(fail bool) func() (int, error) {
		return func() (int, error) {
			calls++
			if fail {
				return 0, errors.New("graph down")
			}
			return calls, nil
		}
	}
	run := WithRunRequester(context.Background(), "ravi", "sana")
	if _, err := RunMemo(run, "k", compute(true)); err == nil {
		t.Fatal("a failure was swallowed")
	}
	first, _ := RunMemo(run, "k", compute(false))
	again, _ := RunMemo(run, "k", compute(false))
	if first != again || calls != 2 {
		t.Errorf("got %v then %v after %d computations, want the second answer kept", first, again, calls)
	}
	calls = 0
	unasked := WithoutRunRequester(context.Background())
	_, _ = RunMemo(unasked, "k", compute(false))
	_, _ = RunMemo(unasked, "k", compute(false))
	if calls != 2 {
		t.Errorf("a run nobody asked for kept an answer (%d computations)", calls)
	}
}

// A run for someone other than the sponsor searches only once what that
// person may not see is known: when deciding it fails, the search fails and
// gets no body, never the sponsor's filter alone.
func TestARunFailsClosedWhenWhatItsAskerMayNotSeeCannotBeDecided(t *testing.T) {
	was := taskread.Decide
	t.Cleanup(func() { taskread.Decide = was })
	failed := errors.New("the asker's restricted tasks could not be decided")
	taskread.Decide = func(_ context.Context, reader string) (map[string]bool, error) {
		if reader == "asker" {
			return nil, failed
		}
		return nil, nil
	}
	const all = `{"query":{"match_all":{}}}`

	// The control: the sponsor's own run never asks about the asker.
	own := WithRunRequester(context.Background(), "sponsor", "sponsor")
	if body, _, err := seenOnly(own, "sponsor", all); err != nil || body == "" {
		t.Fatalf("the sponsor's own run: got body %q, err %v; want the search", body, err)
	}

	asked := WithRunRequester(context.Background(), "asker", "sponsor")
	body, seen, err := seenOnly(asked, "sponsor", all)
	if !errors.Is(err, failed) {
		t.Fatalf("a run the asker asked for searched though their filter failed: err %v", err)
	}
	if body != "" || seen != nil {
		t.Errorf("a failed filter left a search to run: body %q, %d filters", body, len(seen))
	}
}
