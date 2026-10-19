package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

func swapVar[T any](t *testing.T, v *T, x T) {
	t.Helper()
	was := *v
	*v = x
	t.Cleanup(func() { *v = was })
}

func TestTheGraphsChannelsAndGroupsBecomeRows(t *testing.T) {
	ch, maya := uuid.New(), uuid.New()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rows := rowsFor([]conversationDomain.GraphConversation{
		{Kind: models.KindChannel, Ref: ch.String(), CreatedBy: maya.String(), CreatedAt: created},
		{Kind: models.KindGroup, Ref: "0123456789abcdef0123456789abcdef"},
		{Kind: models.KindChannel, Ref: "not-a-uuid"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].ID != ch || *rows[0].CreatedBy != maya || !rows[0].CreatedAt.Equal(created) {
		t.Errorf("a channel's row is its own id: %+v", rows[0])
	}
	if rows[1].ID == uuid.Nil || rows[1].CreatedBy != nil {
		t.Errorf("a group's row gets an id of its own: %+v", rows[1])
	}
}

func TestAChannelsManagersAmongItsMembersAreManagers(t *testing.T) {
	id, a, b, c := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	got := graphMembers(id, conversationDomain.GraphConversation{
		Members:  []string{a.String(), b.String(), a.String(), "nobody"},
		Managers: []string{b.String(), c.String()},
	})
	want := map[uuid.UUID]string{a: models.RoleMember, b: models.RoleManager}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, m := range got {
		if want[m.UserID] != m.Role || m.ConversationID != id {
			t.Errorf("%s: %+v", m.UserID, m)
		}
	}
}

func TestAComparisonRepairsWhatTheIndexHasWrong(t *testing.T) {
	since := time.Now()
	old, recent := since.Add(-time.Hour), since.Add(time.Minute)
	left := old
	in, missing, outInIndex, extra, promote, demote, fresh, freshExtra := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	g := conversationDomain.GraphConversation{
		Members:  []string{in.String(), missing.String(), outInIndex.String(), promote.String(), demote.String(), fresh.String()},
		Managers: []string{promote.String()},
	}
	rows := []models.Member{
		{UserID: in, Role: models.RoleMember, UpdatedAt: old},
		{UserID: outInIndex, Role: models.RoleMember, LeftAt: &left, UpdatedAt: old},
		{UserID: extra, Role: models.RoleMember, UpdatedAt: old},
		{UserID: promote, Role: models.RoleMember, UpdatedAt: old},
		{UserID: demote, Role: models.RoleManager, UpdatedAt: old},
		// Written since the comparison began: the membership functions', left
		// to the next comparison whatever the graph said when it was read.
		{UserID: fresh, Role: models.RoleMember, LeftAt: &recent, UpdatedAt: recent},
		{UserID: freshExtra, Role: models.RoleMember, UpdatedAt: recent},
	}
	r := diff(g, rows, since, since)
	sameSet := func(name string, got []uuid.UUID, want ...uuid.UUID) {
		t.Helper()
		slices.SortFunc(got, func(a, b uuid.UUID) int { return compareUUID(a, b) })
		slices.SortFunc(want, func(a, b uuid.UUID) int { return compareUUID(a, b) })
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	sameSet("join", r.Join, missing, outInIndex)
	sameSet("leave", r.Leave, extra)
	sameSet("promote", r.Promote, promote)
	sameSet("demote", r.Demote, demote)
	if len(r.Manage) != 0 || r.Deferred != 2 || r.Count() != 5 {
		t.Errorf("manage %v deferred %d count %d", r.Manage, r.Deferred, r.Count())
	}
}

func compareUUID(a, b uuid.UUID) int {
	switch {
	case a.String() < b.String():
		return -1
	case a.String() > b.String():
		return 1
	}
	return 0
}

// A comparison records what it repaired, which decides where access decisions
// read next (domain/Conversation source.go: drift reads the graph).
func TestAComparisonRecordsWhatItRepaired(t *testing.T) {
	ch, maya, sam := uuid.New(), uuid.New(), uuid.New()
	g := conversationDomain.GraphConversation{Kind: models.KindChannel, Ref: ch.String(), Members: []string{maya.String()}}
	swapVar(t, &graphPageOf, func(_ context.Context, channels bool, after string, _ int) ([]conversationDomain.GraphConversation, string, error) {
		if channels && after == "" {
			return []conversationDomain.GraphConversation{g}, "", nil
		}
		return nil, "", nil
	})
	swapVar(t, &ensureMany, func(_ context.Context, cs []models.Conversation) (map[string]uuid.UUID, error) {
		out := map[string]uuid.UUID{}
		for _, c := range cs {
			out[c.Ref] = c.ID
		}
		return out, nil
	})
	long := time.Now().Add(-time.Hour)
	index := map[uuid.UUID][]models.Member{ch: {{UserID: sam, Role: models.RoleMember, UpdatedAt: long}}}
	swapVar(t, &rowsOf, func(context.Context, []uuid.UUID) (map[uuid.UUID][]models.Member, error) { return index, nil })
	var joined, leftOut []uuid.UUID
	swapVar(t, &joinMembers, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, _ string, _ time.Time) error {
		joined = append(joined, users...)
		return nil
	})
	swapVar(t, &leaveMembers, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, _ time.Time) error {
		leftOut = append(leftOut, users...)
		return nil
	})
	swapVar(t, &indexRefs, func(context.Context, string, int) ([]models.Conversation, error) {
		return []models.Conversation{{ID: ch, Ref: ch.String()}}, nil
	})
	var settled []int
	swapVar(t, &settle, func(_ context.Context, from string, _ time.Time, repaired int, _ string) (bool, error) {
		if from != "clean|before" {
			t.Errorf("settled from %q", from)
		}
		settled = append(settled, repaired)
		return true, nil
	})

	rep, err := Reconcile(context.Background(), "clean|before")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(joined, []uuid.UUID{maya}) || !slices.Equal(leftOut, []uuid.UUID{sam}) {
		t.Fatalf("joined %v, left %v", joined, leftOut)
	}
	if rep.Repaired != 2 || !slices.Equal(settled, []int{2}) || !rep.Settled {
		t.Fatalf("report %+v, settled %v", rep, settled)
	}

	// The next comparison, with the index repaired, finds nothing.
	index = map[uuid.UUID][]models.Member{ch: {{UserID: maya, Role: models.RoleMember, UpdatedAt: long}}}
	joined, leftOut = nil, nil
	if rep, err = Reconcile(context.Background(), "clean|before"); err != nil || rep.Repaired != 0 || !slices.Equal(settled, []int{2, 0}) {
		t.Fatalf("report %+v, settled %v, err %v", rep, settled, err)
	}
}

// A conversation the graph no longer has keeps nobody in the index.
func TestAConversationGoneFromTheGraphKeepsNobody(t *testing.T) {
	gone, sam := uuid.New(), uuid.New()
	swapVar(t, &graphPageOf, func(context.Context, bool, string, int) ([]conversationDomain.GraphConversation, string, error) {
		return nil, "", nil
	})
	swapVar(t, &indexRefs, func(_ context.Context, after string, _ int) ([]models.Conversation, error) {
		if after != "" {
			return nil, nil
		}
		return []models.Conversation{{ID: gone, Ref: "abcdef0123456789abcdef0123456789"}}, nil
	})
	swapVar(t, &rowsOf, func(context.Context, []uuid.UUID) (map[uuid.UUID][]models.Member, error) {
		return map[uuid.UUID][]models.Member{gone: {{UserID: sam, Role: models.RoleMember, UpdatedAt: time.Now().Add(-time.Hour)}}}, nil
	})
	var leftOut []uuid.UUID
	swapVar(t, &leaveMembers, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, _ time.Time) error {
		leftOut = append(leftOut, users...)
		return nil
	})
	swapVar(t, &joinMembers, func(context.Context, uuid.UUID, []uuid.UUID, string, time.Time) error { return nil })
	swapVar(t, &settle, func(context.Context, string, time.Time, int, string) (bool, error) { return true, nil })
	rep, err := Reconcile(context.Background(), "")
	if err != nil || rep.Gone != 1 || !slices.Equal(leftOut, []uuid.UUID{sam}) {
		t.Fatalf("report %+v, left %v, err %v", rep, leftOut, err)
	}
}

// The job fills the index in once, compares it at the next pass, then only
// when the last clean comparison is a day old.
func TestTheJobFillsTheIndexInOnceThenComparesWhenDue(t *testing.T) {
	done := false
	var comparisons int
	swapVar(t, &backfillDone, func() (bool, error) { return done, nil })
	swapVar(t, &markBackfill, func(string) error { done = true; return nil })
	swapVar(t, &graphPageOf, func(context.Context, bool, string, int) ([]conversationDomain.GraphConversation, string, error) {
		return nil, "", nil
	})
	swapVar(t, &indexRefs, func(context.Context, string, int) ([]models.Conversation, error) {
		comparisons++
		return nil, nil
	})
	state := ""
	swapVar(t, &stateNow, func(context.Context) (string, error) { return state, nil })
	swapVar(t, &settle, func(_ context.Context, _ string, began time.Time, _ int, _ string) (bool, error) {
		state = conversationDomain.State{Kind: conversationDomain.StateClean, At: began}.String()
		return true, nil
	})
	after := time.Now().Add(-48 * time.Hour)
	swapVar(t, &dueAfter, func(context.Context) (time.Time, error) { return after, nil })
	ctx := context.Background()
	// The pass that fills it in leaves the comparing to the next, once the
	// rows it wrote are past the margin.
	if err := runIndex(ctx); err != nil || !done || comparisons != 0 {
		t.Fatalf("first run: done %v, comparisons %d, err %v", done, comparisons, err)
	}
	if err := runIndex(ctx); err != nil || comparisons != 1 {
		t.Fatalf("the pass after the backfill didn't compare: %d", comparisons)
	}
	if err := runIndex(ctx); err != nil || comparisons != 1 {
		t.Fatalf("a clean index was compared again at once: %d", comparisons)
	}
	state = conversationDomain.State{Kind: conversationDomain.StateClean, At: time.Now().Add(-25 * time.Hour)}.String()
	if err := runIndex(ctx); err != nil || comparisons != 2 {
		t.Fatalf("a day-old clean comparison wasn't repeated: %d", comparisons)
	}
	state = conversationDomain.State{Kind: conversationDomain.StateDirty, At: time.Now()}.String()
	if err := runIndex(ctx); err != nil || comparisons != 3 {
		t.Fatalf("a dirty index wasn't compared: %d", comparisons)
	}

	// A clean comparison an hour old began before this process (or one that
	// asked) started: a restored database brings such a stamp back, so it is
	// compared again at once; the comparison it makes is since the start.
	state = conversationDomain.State{Kind: conversationDomain.StateClean, At: time.Now().Add(-time.Hour)}.String()
	after = time.Now().Add(-time.Minute)
	if err := runIndex(ctx); err != nil || comparisons != 4 {
		t.Fatalf("a clean comparison from before the start wasn't repeated: %d", comparisons)
	}
	if err := runIndex(ctx); err != nil || comparisons != 4 {
		t.Fatalf("a comparison since the start was repeated: %d", comparisons)
	}
	swapVar(t, &dueAfter, func(context.Context) (time.Time, error) { return time.Time{}, errors.New("connection refused") })
	if err := runIndex(ctx); err == nil {
		t.Fatal("a request that couldn't be read was taken for none")
	}
}

// The job runs when a worker starts, not ten minutes later: a process trusts
// no comparison from before it started.
func TestTheIndexJobRunsWhenTheSweepsStart(t *testing.T) {
	if indexJob.Name != "conversations.index" || !indexJob.RunAtStart || indexJob.Every != indexEvery {
		t.Fatalf("the job: %+v", indexJob)
	}
}

// A comparison leaves to the next one the rows written since it began (less
// the margin), but not a row from before this process started, however recent:
// after a restore that row came from the backup, and nothing here is writing it.
func TestARowFromBeforeTheProcessStartedIsCompared(t *testing.T) {
	ch, maya, sam, olu := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	start := time.Now().Add(-30 * time.Second)
	swapVar(t, &startedAt, func() time.Time { return start })
	g := conversationDomain.GraphConversation{Kind: models.KindChannel, Ref: ch.String(), Members: []string{maya.String()}}
	swapVar(t, &graphPageOf, func(_ context.Context, channels bool, after string, _ int) ([]conversationDomain.GraphConversation, string, error) {
		if channels && after == "" {
			return []conversationDomain.GraphConversation{g}, "", nil
		}
		return nil, "", nil
	})
	swapVar(t, &ensureMany, func(_ context.Context, cs []models.Conversation) (map[string]uuid.UUID, error) {
		return map[string]uuid.UUID{ch.String(): ch}, nil
	})
	// Both inside the margin: Sam's from before the start, Olu's since.
	index := map[uuid.UUID][]models.Member{ch: {
		{UserID: maya, Role: models.RoleMember, UpdatedAt: start.Add(-time.Hour)},
		{UserID: sam, Role: models.RoleMember, UpdatedAt: start.Add(-10 * time.Second)},
		{UserID: olu, Role: models.RoleMember, UpdatedAt: start.Add(10 * time.Second)},
	}}
	swapVar(t, &rowsOf, func(context.Context, []uuid.UUID) (map[uuid.UUID][]models.Member, error) { return index, nil })
	var leftOut []uuid.UUID
	swapVar(t, &leaveMembers, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, _ time.Time) error {
		leftOut = append(leftOut, users...)
		return nil
	})
	swapVar(t, &joinMembers, func(context.Context, uuid.UUID, []uuid.UUID, string, time.Time) error { return nil })
	swapVar(t, &indexRefs, func(context.Context, string, int) ([]models.Conversation, error) { return nil, nil })
	var stamped time.Time
	swapVar(t, &settle, func(_ context.Context, _ string, began time.Time, _ int, _ string) (bool, error) {
		stamped = began
		return true, nil
	})
	before := time.Now()
	rep, err := Reconcile(context.Background(), "clean|before")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(leftOut, []uuid.UUID{sam}) || rep.Deferred != 1 || rep.Repaired != 1 {
		t.Fatalf("left %v, report %+v; want Sam out and Olu's row left to the next", leftOut, rep)
	}
	if stamped.Before(before) || stamped.After(time.Now()) {
		t.Fatalf("settled with %v, not when the comparison began", stamped)
	}
}

// The demo's first boot (17 Oct): the backfill wrote every row, and a
// comparison at once left them all to the next one, written inside its margin,
// yet called the index clean; cleanFor then put the next real comparison a day
// away. Every nightly reset backfills again. A comparison that left rows it was
// due to check isn't clean: the state stays unchecked, and the next pass, once
// the rows are past the margin, compares them and finds the index clean.
func TestABackfillThenACompareAtOnceLeavesTheIndexUnchecked(t *testing.T) {
	ch, maya, sam := uuid.New(), uuid.New(), uuid.New()
	swapVar(t, &startedAt, func() time.Time { return time.Now().Add(-time.Hour) })
	swapVar(t, &dueAfter, func(context.Context) (time.Time, error) { return time.Now().Add(-time.Hour), nil })
	g := conversationDomain.GraphConversation{Kind: models.KindChannel, Ref: ch.String(), Members: []string{maya.String(), sam.String()}}
	swapVar(t, &graphPageOf, func(_ context.Context, channels bool, after string, _ int) ([]conversationDomain.GraphConversation, string, error) {
		if channels && after == "" {
			return []conversationDomain.GraphConversation{g}, "", nil
		}
		return nil, "", nil
	})
	swapVar(t, &ensureMany, func(context.Context, []models.Conversation) (map[string]uuid.UUID, error) {
		return map[string]uuid.UUID{ch.String(): ch}, nil
	})
	// The rows as the backfill wrote them, at written.
	written := time.Now()
	swapVar(t, &rowsOf, func(context.Context, []uuid.UUID) (map[uuid.UUID][]models.Member, error) {
		return map[uuid.UUID][]models.Member{ch: {
			{UserID: maya, Role: models.RoleMember, UpdatedAt: written},
			{UserID: sam, Role: models.RoleMember, UpdatedAt: written},
		}}, nil
	})
	swapVar(t, &insertAbsent, func(_ context.Context, ms []models.Member) (int64, error) { return int64(len(ms)), nil })
	swapVar(t, &indexRefs, func(context.Context, string, int) ([]models.Conversation, error) {
		return []models.Conversation{{ID: ch, Ref: ch.String()}}, nil
	})
	done := false
	swapVar(t, &backfillDone, func() (bool, error) { return done, nil })
	swapVar(t, &markBackfill, func(string) error { done = true; return nil })
	state := ""
	swapVar(t, &stateNow, func(context.Context) (string, error) { return state, nil })
	swapVar(t, &settle, func(_ context.Context, from string, began time.Time, repaired int, _ string) (bool, error) {
		if from != state {
			return false, nil
		}
		kind := conversationDomain.StateClean
		if repaired > 0 {
			kind = conversationDomain.StateDrift
		}
		state = conversationDomain.State{Kind: kind, At: began}.String()
		return true, nil
	})
	ctx := context.Background()

	// The pass that fills the index in doesn't call it clean.
	if err := runIndex(ctx); err != nil || !done {
		t.Fatalf("the first pass: done %v, err %v", done, err)
	}
	if state != "" {
		t.Fatalf("the pass that backfilled left the state %q, want it unchecked", state)
	}
	// Nor does a comparison at once: every row was written moments before it
	// began, and it checked none of them.
	rep, err := Reconcile(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if state != "" || rep.Settled || rep.Deferred != 2 {
		t.Fatalf("a comparison that checked no row settled the state %q (report %s)", state, rep)
	}
	// A tick later the rows are past the margin: the next pass compares them,
	// and the index is clean.
	written = time.Now().Add(-indexEvery)
	if err := runIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := conversationDomain.ParseState(state); !ok || s.Kind != conversationDomain.StateClean {
		t.Fatalf("the comparison that checked every row left the state %q", state)
	}
}

// Of the rows a comparison leaves, the ones written before it began are rows
// it was due to check: they keep it from calling the index clean. The ones
// written since it began are newer than what it checked.
func TestRowsLeftFromBeforeAComparisonBeganAreUnchecked(t *testing.T) {
	began := time.Now()
	since := began.Add(-writeMargin)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	g := conversationDomain.GraphConversation{Members: []string{a.String(), b.String()}}
	r := diff(g, []models.Member{
		{UserID: a, Role: models.RoleMember, UpdatedAt: began.Add(-time.Minute)},      // inside the margin, before it began
		{UserID: b, Role: models.RoleMember, UpdatedAt: began.Add(time.Second)},       // since it began
		{UserID: c, Role: models.RoleMember, UpdatedAt: began.Add(-30 * time.Second)}, // not in the graph, inside the margin
	}, since, began)
	if r.Deferred != 3 || r.Unchecked != 2 || r.Count() != 0 {
		t.Fatalf("deferred %d, unchecked %d, repairs %d", r.Deferred, r.Unchecked, r.Count())
	}
}
