package business

// The Work Change Log (WCL-1): each change to work numbered in task_changes,
// read back by Since for whoever may see it, and told to its project's open
// views. Record runs after a write is stored and never fails it. The entries
// sinks want go through the Outbox with them (sinks.go).

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
)

// Change is one change to log: what it changed (a task first), its project,
// the fields' new values in the app's own answer names, who made it and from
// where. Restricted says only a few may see the task (WA2).
type Change struct {
	Kind         string
	ResourceKind string
	ResourceUUID string
	ProjectUUID  string
	Diff         json.RawMessage
	Actor        string
	Source       string
	Restricted   bool
	At           time.Time
}

// Row is one change as task_changes keeps it.
type Row struct {
	Sequence int64
	Change
}

// MaxSince is the most entries one answer holds.
const MaxSince = 500

var (
	// ErrCursorExpired is a place older than the oldest change kept: the
	// reader reads its lists again and starts from the head.
	ErrCursorExpired = errors.New("work feed: that place in the log is gone; read your lists again")
	// ErrBadCursor is a place that isn't one.
	ErrBadCursor = errors.New("work feed: that isn't a place in the log")
	// ErrNoSight is the feed before business/Task says who may see what.
	ErrNoSight = errors.New("work feed: who may see what isn't known yet")
)

// Seams: the store (task_changes), the reader's sight (business/Task
// registers it) and the project's topic.
var (
	appendRows    = appendToStore
	rowsAfter     = readFromStore
	headAndOldest = taskChangeModels.HeadAndOldest
	mayRead       = closedSight
	publishOne    = publishTask
	publishReread = mqttBusiness.PublishProjectReread
)

// MaxToldOneByOne is the most entries of one project told one by one. A
// bigger burst (a renumbered column, a pasted list) is told as one Reread,
// and the project's open views read their lists again.
const MaxToldOneByOne = 25

// appendToStore keeps changes in task_changes and, in the same transaction,
// writes the Outbox events of those a sink wants (sinks.go): an entry and its
// event are kept together or not at all. Once they are committed, the events
// are released and delivered (handOn).
func appendToStore(ctx context.Context, cs []Change) ([]int64, error) {
	rows := make([]taskChangeModels.New, len(cs))
	for i, c := range cs {
		rows[i] = taskChangeModels.New{ProjectUUID: c.ProjectUUID, ResourceKind: c.ResourceKind, ResourceUUID: c.ResourceUUID,
			Kind: c.Kind, Diff: c.Diff, ActorUUID: c.Actor, Source: c.Source, Restricted: c.Restricted, At: c.At}
	}
	var seqs, events []int64
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var err error
		if seqs, err = taskChangeModels.Append(ctx, rows); err != nil {
			return err
		}
		es := eventsFor(ctx, cs, seqs)
		if len(es) == 0 {
			return nil
		}
		tx, err := pgexec.Current(ctx)
		if err != nil {
			return err
		}
		events, err = outboxBusiness.AddMany(ctx, tx, es)
		return err
	})
	if err != nil {
		return nil, err
	}
	handOn(ctx, events)
	return seqs, nil
}

func readFromStore(ctx context.Context, after int64, limit int) ([]Row, error) {
	kept, err := taskChangeModels.After(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, len(kept))
	for i, k := range kept {
		rows[i] = Row{Sequence: k.Sequence, Change: Change{Kind: k.Kind, ResourceKind: k.ResourceKind, ResourceUUID: k.ResourceUUID,
			ProjectUUID: k.ProjectUUID, Diff: k.Diff, Actor: k.ActorUUID, Source: k.Source, Restricted: k.Restricted, At: k.At}}
	}
	return rows, nil
}

// closedSight is the feed until business/Task says who may see what: closed.
func closedSight(context.Context, accessBusiness.Subject, []Row) (map[int64]bool, error) {
	return nil, ErrNoSight
}

// RegisterSight is how business/Task says which rows a reader may see: their
// projects through Work Access, restricted tasks through the WA2 filter. A
// sight that can't answer returns an error, and Since answers nothing.
func RegisterSight(sees func(ctx context.Context, who accessBusiness.Subject, rows []Row) (map[int64]bool, error)) {
	if sees != nil {
		mayRead = sees
	}
}

// publishTask tells a task's project of its entry; a restricted task's names
// nothing (business/Mqtt's one rule).
func publishTask(ctx context.Context, e Entry) {
	if e.ResourceKind != "task" {
		return
	}
	mqttBusiness.PublishWorkChange(ctx, mqttBusiness.WorkChange{Sequence: e.Sequence, Kind: e.Kind,
		TaskUUID: e.ResourceUUID, ProjectUUID: e.ProjectUUID, Diff: e.Diff, By: e.Actor})
}

// Log is the Work Change Log.
var Log Feed = workLog{}

type workLog struct{}

// Record numbers and keeps changes, in the order given, then tells their
// projects. A change the log can't keep is logged as an error and told to
// nobody; the write that made it stands.
func Record(ctx context.Context, changes ...Change) []Entry {
	if len(changes) == 0 {
		return nil
	}
	seqs, err := appendRows(ctx, changes)
	if err == nil && len(seqs) != len(changes) {
		err = errors.New("the log numbered a different count")
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/WorkFeed Record: %d changes not logged: %v", len(changes), err)
		return nil
	}
	entries := make([]Entry, len(changes))
	for i, c := range changes {
		entries[i] = entryOf(Row{Sequence: seqs[i], Change: c})
	}
	Log.Publish(ctx, entries)
	return entries
}

func entryOf(r Row) Entry {
	return Entry{Sequence: r.Sequence, Kind: r.Kind, ResourceKind: r.ResourceKind, ResourceUUID: r.ResourceUUID,
		ProjectUUID: r.ProjectUUID, Diff: r.Diff, Actor: r.Actor, Source: r.Source, At: r.At, Restricted: r.Restricted}
}

// Since answers what who may see after cursor, in order, and the place to ask
// from next, which moves past rows they may not see. No place answers the
// head: the reader has just read its lists whole. A place older than the
// oldest kept, or past the head (a database restored from a backup), is
// gone: the reader reads its lists again. The rows are read before the ends
// are, so a prune between the two only makes the answer stricter: a reader
// never steps over changes a prune took before they were read.
func (workLog) Since(ctx context.Context, who accessBusiness.Subject, cursor Cursor, limit int) ([]Entry, Cursor, error) {
	if cursor == "" {
		head, _, err := headAndOldest(ctx)
		if err != nil {
			return nil, cursor, err
		}
		return nil, placeOf(head), nil
	}
	after, err := strconv.ParseInt(string(cursor), 10, 64)
	if err != nil || after < 0 {
		return nil, cursor, ErrBadCursor
	}
	if limit <= 0 || limit > MaxSince {
		limit = MaxSince
	}
	rows, err := rowsAfter(ctx, after, limit)
	if err != nil {
		return nil, cursor, err
	}
	head, oldest, err := headAndOldest(ctx)
	if err != nil {
		return nil, cursor, err
	}
	if after > head || (oldest > 0 && after < oldest-1) {
		return nil, cursor, ErrCursorExpired
	}
	if len(rows) == 0 {
		return nil, cursor, nil
	}
	sees, err := mayRead(ctx, who, rows)
	if err != nil {
		return nil, cursor, err
	}
	var out []Entry
	for _, r := range rows {
		if sees[r.Sequence] {
			out = append(out, entryOf(r))
		}
	}
	return out, placeOf(rows[len(rows)-1].Sequence), nil
}

// Publish tells each entry's project of it, one by one, or once as Reread
// for a project with more than MaxToldOneByOne of them. It reads no
// restriction: each entry's came with its batch's one read (RecordTasks). An
// open task is marked so (helpers.WithOpenTasks) and business/Mqtt's rule
// takes the mark; a restricted task's change is its project's Reread, naming
// nothing, as that rule tells it.
func (workLog) Publish(ctx context.Context, entries []Entry) {
	count := map[string]int{}
	open := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		count[e.ProjectUUID]++
		if id, err := uuid.Parse(e.ResourceUUID); err == nil && !e.Restricted {
			open = append(open, id)
		}
	}
	ctx = helpers.WithOpenTasks(ctx, open...)
	told := map[string]bool{}
	for _, e := range entries {
		if count[e.ProjectUUID] <= MaxToldOneByOne && !e.Restricted {
			publishOne(ctx, e)
			continue
		}
		if !told[e.ProjectUUID] {
			told[e.ProjectUUID] = true
			publishReread(ctx, e.ProjectUUID)
		}
	}
}

func placeOf(seq int64) Cursor { return Cursor(strconv.FormatInt(seq, 10)) }
