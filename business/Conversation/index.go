package business

// The conversations index (Postgres: conversations, conversation_members) is
// filled in from the graph once, kept beside it by the membership functions
// (domain/Conversation members.go), and compared with it: by one Sweeper job,
// conversations.index, which
//   - fills the index in from the graph until that has been done once
//     (Backfill), then
//   - compares every conversation's people in the graph with the index and
//     repairs the index where they differ (Reconcile), whenever the index isn't
//     known to be clean, or was last found clean a day ago, or before this
//     process (or another that asked) started.
//
// It runs when a worker starts as well as every ten minutes: a process trusts
// no comparison that began before it started, since a database restored from
// a backup brings back an older comparison's "clean" beside a newer graph
// (domain/Conversation source.go).
//
// The graph is the truth both read. What the comparison finds decides where
// access decisions read (domain/Conversation source.go): nothing to repair, the
// index; anything repaired, the graph until a comparison finds nothing.
//
// A row written while a comparison runs is the membership functions' doing,
// which keeps the index in the order that never grants more than the graph: the
// comparison leaves alone any row changed after it began (less a margin for
// the clocks of the processes that write), and the next one looks at it. A row
// from before this process started is no write in progress here, so it is
// compared, however recent: after a restore it came from the backup.
//
// ONLY A COMPARISON THAT CHECKED EVERY ROW IT WAS DUE TO MAY CALL THE INDEX
// CLEAN (17 Oct 2026). On the demo's first boot the backfill wrote every row,
// a comparison at once left them all to the next one, inside its margin, and
// still wrote "clean"; cleanFor put the next real comparison a day away, and
// every nightly reset backfills again. A row the comparison left that was
// written before it began is one it was due to check and didn't (Unchecked):
// such a comparison leaves the state as it was, unchecked stays unchecked, and
// the next tick, past the margin, compares again. Rows written after it began
// are newer than what it checked, the membership functions' writes in their
// safe order, and don't stop it. The pass that backfills doesn't compare at
// all: its rows carry the database's clock, which may be ahead of this one.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

const (
	// indexEvery is how often the job looks at the index.
	indexEvery = 10 * time.Minute
	// cleanFor is how long a clean comparison is trusted before the next.
	cleanFor = 24 * time.Hour
	// graphPage is how many conversations of the graph one read takes.
	graphPage = 200
	// indexPage is how many of the index's conversations one read takes.
	indexPage = 1000
	// writeMargin is how recently a row may have been written for a
	// comparison to leave it to the next one: the comparison's own start,
	// less this for the clocks of the processes that write rows.
	writeMargin = 2 * time.Minute
)

// indexJob is the job, first run when the sweeps start.
var indexJob = sweepBusiness.Job{Name: "conversations.index", Every: indexEvery, Run: runIndex, RunAtStart: true}

func init() {
	sweepBusiness.Jobs.Register(indexJob)
}

// What the job reads and writes, seams for tests.
var (
	graphPageOf   = conversationDomain.GraphPage
	ensureMany    = models.EnsureMany
	rowsOf        = models.RowsOf
	insertAbsent  = models.InsertIfAbsent
	joinMembers   = models.Join
	leaveMembers  = models.Leave
	setMemberRole = models.SetRole
	indexRefs     = models.RefsPage
	backfillDone  = func() (bool, error) { return configModel.IsDone(models.BackfillKey) }
	markBackfill  = func(note string) error { return configModel.UpsertConfig(models.BackfillKey, "done: "+note) }
	stateNow      = conversationDomain.StateNow
	settle        = conversationDomain.Settle
	dueAfter      = conversationDomain.ComparisonDueAfter
	startedAt     = conversationDomain.StartedAt
)

// runIndex is the job: the backfill until it has been done, then a comparison
// when one is due.
func runIndex(ctx context.Context) error {
	done, err := backfillDone()
	if err != nil {
		return fmt.Errorf("reading whether the conversations index is filled in: %w", err)
	}
	if !done {
		rep, err := Backfill(ctx)
		if err != nil {
			return fmt.Errorf("filling in the conversations index (so far %s): %w", rep, err)
		}
		if err := markBackfill(rep.String()); err != nil {
			return err
		}
		helpers.LogInfoWithContext(ctx, "Conversations index filled in from the graph: %s; the next pass compares it, once its rows are past the margin", rep)
		return nil
	}
	from, err := stateNow(ctx)
	if err != nil {
		return err
	}
	after, err := dueAfter(ctx)
	if err != nil {
		return err
	}
	if s, ok := conversationDomain.ParseState(from); ok && s.Kind == conversationDomain.StateClean && time.Since(s.At) < cleanFor && !s.At.Before(after) {
		return nil
	}
	rep, err := Reconcile(ctx, from)
	if err != nil {
		return fmt.Errorf("comparing the conversations index with the graph (so far %s): %w", rep, err)
	}
	helpers.LogInfoWithContext(ctx, "Conversations index compared with the graph: %s", rep)
	return nil
}

// BackfillReport is what a backfill wrote.
type BackfillReport struct {
	Conversations int
	Members       int64
}

func (r BackfillReport) String() string {
	return fmt.Sprintf("%d conversations, %d people's places written", r.Conversations, r.Members)
}

// Backfill writes into the index every conversation the graph has and the
// people in it, leaving every row already there as it is (a join or a removal
// written beside it outranks what the graph said when it was read).
func Backfill(ctx context.Context) (BackfillReport, error) {
	var rep BackfillReport
	err := eachGraphPage(ctx, func(page []conversationDomain.GraphConversation) error {
		ids, err := ensureMany(ctx, rowsFor(page))
		if err != nil {
			return err
		}
		var members []models.Member
		for _, c := range page {
			id, ok := ids[c.Ref]
			if !ok {
				continue
			}
			members = append(members, graphMembers(id, c)...)
		}
		n, err := insertAbsent(ctx, members)
		rep.Conversations += len(page)
		rep.Members += n
		return err
	})
	return rep, err
}

// eachGraphPage reads every conversation in the graph, channels first, a page
// at a time.
func eachGraphPage(ctx context.Context, fn func([]conversationDomain.GraphConversation) error) error {
	for _, channels := range []bool{true, false} {
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, next, err := graphPageOf(ctx, channels, after, graphPage)
			if err != nil {
				return err
			}
			if len(page) > 0 {
				if err := fn(page); err != nil {
					return err
				}
			}
			if next == "" {
				break
			}
			after = next
		}
	}
	return nil
}

// rowsFor is the conversations rows a page of the graph names. Pure.
func rowsFor(page []conversationDomain.GraphConversation) []models.Conversation {
	out := make([]models.Conversation, 0, len(page))
	for _, c := range page {
		row := models.Conversation{Kind: c.Kind, Ref: c.Ref, CreatedAt: c.CreatedAt, ID: uuid.New()}
		if c.Kind == models.KindChannel {
			id, err := uuid.Parse(c.Ref)
			if err != nil {
				continue
			}
			row.ID = id
		}
		if by, err := uuid.Parse(c.CreatedBy); err == nil {
			row.CreatedBy = &by
		}
		out = append(out, row)
	}
	return out
}

// graphMembers is the rows the graph's people of c make: a channel's managers
// who are its members as managers. Pure.
func graphMembers(id uuid.UUID, c conversationDomain.GraphConversation) []models.Member {
	managers := map[string]bool{}
	for _, m := range c.Managers {
		managers[strings.ToLower(m)] = true
	}
	var out []models.Member
	seen := map[uuid.UUID]bool{}
	for _, m := range c.Members {
		u, err := uuid.Parse(m)
		if err != nil || seen[u] {
			continue
		}
		seen[u] = true
		role := models.RoleMember
		if managers[strings.ToLower(m)] {
			role = models.RoleManager
		}
		out = append(out, models.Member{ConversationID: id, UserID: u, Role: role, JoinedAt: c.CreatedAt})
	}
	return out
}

// Repairs is what a comparison changes in the index for one conversation.
type Repairs struct {
	Join     []uuid.UUID // in the graph, not in the index
	Manage   []uuid.UUID // in the graph as managers, not in the index (or out of it)
	Leave    []uuid.UUID // in the index, not in the graph
	Promote  []uuid.UUID // managers in the graph, members in the index
	Demote   []uuid.UUID // members in the graph, managers in the index
	Deferred int         // rows written since the comparison began (less the margin), left to the next
	// Unchecked is how many of the Deferred were written before the comparison
	// began: rows it was due to check and didn't.
	Unchecked int
}

// Count is how many differences there are.
func (r Repairs) Count() int {
	return len(r.Join) + len(r.Manage) + len(r.Leave) + len(r.Promote) + len(r.Demote)
}

// diff is what makes the index's rows of a conversation say what the graph
// does, for a comparison that began at began. A row written since since is
// left alone. Pure.
func diff(c conversationDomain.GraphConversation, rows []models.Member, since, began time.Time) Repairs {
	var r Repairs
	leave := func(row models.Member) {
		r.Deferred++
		if row.UpdatedAt.Before(began) {
			r.Unchecked++
		}
	}
	inGraph := map[uuid.UUID]bool{}
	manager := map[uuid.UUID]bool{}
	for _, m := range c.Managers {
		if u, err := uuid.Parse(m); err == nil {
			manager[u] = true
		}
	}
	for _, m := range c.Members {
		if u, err := uuid.Parse(m); err == nil {
			inGraph[u] = true
		}
	}
	byUser := map[uuid.UUID]models.Member{}
	for _, row := range rows {
		byUser[row.UserID] = row
	}
	for _, u := range sortedKeys(inGraph) {
		row, have := byUser[u]
		switch {
		case have && !row.UpdatedAt.Before(since):
			leave(row)
		case !have || !row.Live():
			if manager[u] {
				r.Manage = append(r.Manage, u)
			} else {
				r.Join = append(r.Join, u)
			}
		case manager[u] && row.Role != models.RoleManager:
			r.Promote = append(r.Promote, u)
		case !manager[u] && row.Role == models.RoleManager:
			r.Demote = append(r.Demote, u)
		}
	}
	for _, row := range rows {
		if !row.Live() || inGraph[row.UserID] {
			continue
		}
		if !row.UpdatedAt.Before(since) {
			leave(row)
			continue
		}
		r.Leave = append(r.Leave, row.UserID)
	}
	slices.SortFunc(r.Leave, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return r
}

func sortedKeys(m map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// ReconcileReport is what a comparison found.
type ReconcileReport struct {
	Conversations int
	Repaired      int
	Deferred      int
	Unchecked     int // of the Deferred, rows written before it began
	Gone          int // conversations the graph no longer has, their people taken out
	Settled       bool
}

func (r ReconcileReport) String() string {
	s := fmt.Sprintf("%d conversations compared, %d differences repaired, %d left to the next comparison, %d no longer in the graph", r.Conversations, r.Repaired, r.Deferred, r.Gone)
	switch {
	case r.Unchecked > 0 && r.Repaired == 0:
		s += fmt.Sprintf("; %d of the rows left were written before it began, so it checked less than all and the index stays unchecked until the next comparison", r.Unchecked)
	case !r.Settled:
		s += "; the index changed while it ran, so it stays unchecked"
	}
	return s
}

// Reconcile compares every conversation's people in the graph with the
// index's, repairs the index where they differ, and records what it found
// (unless the index's state changed from from while it ran).
func Reconcile(ctx context.Context, from string) (ReconcileReport, error) {
	var rep ReconcileReport
	began := time.Now()
	since := began.Add(-writeMargin)
	if start := startedAt(); start.After(since) {
		since = start
	}
	seen := map[string]bool{}
	var notes []string
	note := func(s string) {
		if len(notes) < 5 {
			notes = append(notes, s)
		}
	}
	err := eachGraphPage(ctx, func(page []conversationDomain.GraphConversation) error {
		ids, err := ensureMany(ctx, rowsFor(page))
		if err != nil {
			return err
		}
		convs := make([]uuid.UUID, 0, len(ids))
		for _, id := range ids {
			convs = append(convs, id)
		}
		rows, err := rowsOf(ctx, convs)
		if err != nil {
			return err
		}
		for _, c := range page {
			seen[c.Ref] = true
			id, ok := ids[c.Ref]
			if !ok {
				continue
			}
			rep.Conversations++
			r := diff(c, rows[id], since, began)
			rep.Deferred += r.Deferred
			rep.Unchecked += r.Unchecked
			if r.Count() == 0 {
				continue
			}
			if err := apply(ctx, id, r); err != nil {
				return err
			}
			rep.Repaired += r.Count()
			note(fmt.Sprintf("%s %s: %d", c.Kind, c.Ref, r.Count()))
		}
		return nil
	})
	if err == nil {
		err = leaveGone(ctx, seen, since, began, &rep, note)
	}
	if err != nil {
		return rep, err
	}
	if rep.Unchecked > 0 && rep.Repaired == 0 {
		// It left rows it was due to check: whatever it found, it can't say
		// the index is clean. The state stays as it was, and the next tick
		// compares again.
		return rep, nil
	}
	ok, err := settle(ctx, from, began, rep.Repaired, strings.Join(notes, ", "))
	rep.Settled = ok
	return rep, err
}

// apply makes the repairs.
func apply(ctx context.Context, id uuid.UUID, r Repairs) error {
	now := time.Now()
	if err := joinMembers(ctx, id, r.Join, models.RoleMember, now); err != nil {
		return err
	}
	if err := joinMembers(ctx, id, r.Manage, models.RoleManager, now); err != nil {
		return err
	}
	if err := leaveMembers(ctx, id, r.Leave, now); err != nil {
		return err
	}
	for _, u := range r.Promote {
		if err := setMemberRole(ctx, id, u, models.RoleManager, now); err != nil {
			return err
		}
	}
	for _, u := range r.Demote {
		if err := setMemberRole(ctx, id, u, models.RoleMember, now); err != nil {
			return err
		}
	}
	return nil
}

// leaveGone takes everyone out of the index's conversations the graph no
// longer has.
func leaveGone(ctx context.Context, seen map[string]bool, since, began time.Time, rep *ReconcileReport, note func(string)) error {
	after := ""
	for {
		page, err := indexRefs(ctx, after, indexPage)
		if err != nil {
			return err
		}
		var gone []uuid.UUID
		refOf := map[uuid.UUID]string{}
		for _, c := range page {
			if !seen[c.Ref] {
				gone = append(gone, c.ID)
				refOf[c.ID] = c.Ref
			}
		}
		if len(gone) > 0 {
			rows, err := rowsOf(ctx, gone)
			if err != nil {
				return err
			}
			for _, id := range gone {
				r := diff(conversationDomain.GraphConversation{Ref: refOf[id]}, rows[id], since, began)
				rep.Deferred += r.Deferred
				rep.Unchecked += r.Unchecked
				if r.Count() == 0 {
					continue
				}
				if err := apply(ctx, id, r); err != nil {
					return err
				}
				rep.Gone++
				rep.Repaired += r.Count()
				note(fmt.Sprintf("%s: gone from the graph", refOf[id]))
			}
		}
		if len(page) < indexPage {
			return nil
		}
		after = page[len(page)-1].Ref
	}
}
