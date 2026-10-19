package domain

// Where access decisions read who is in a conversation: the graph's membership
// edges, as every check has always read them, or the conversations index in
// Postgres (conversation_members), which decides a whole list in one query.
//
// The index is read only while it is known to agree with the graph:
//   - it has been filled in from the graph (the backfill's "done" mark), and
//   - the last comparison with the graph found nothing to repair ("clean"), and
//   - that comparison began after this process started, and
//   - no write to it has failed since ("dirty" otherwise), and
//   - CONVERSATION_ACCESS_SOURCE isn't "graph", which an operator sets to keep
//     every decision on the graph whatever the index says.
//
// WHY A PROCESS TRUSTS NO COMPARISON OLDER THAN ITSELF (17 Oct 2026). A backup
// is taken online: Postgres first, the graph after it. Restored, the index and
// the "clean" its last comparison wrote come back from the first moment, beside
// a graph from the second. Someone taken out of a private channel or a group
// between the two is in that index and out of that graph, and the old "clean"
// would let them in. make restore marks the state dirty (Makefile-distribute,
// RESTORED_INDEX_STATE), but a database can be put back by other means, and a
// process can't tell a restored stamp from a real one; it can tell that a stamp
// is older than itself. So a process reads the graph until a comparison that
// began after it started finds the index clean. The comparison job runs at a
// worker's start (business/Conversation index.go); a process that runs no
// sweeps (SERVICE_ROLE=api) asks for one (askForComparison), which the next
// worker tick runs. The cost, once for each start: one comparison, which reads
// the graph's conversations 200 a page with one or two Postgres reads a page,
// and the index's 1,000 a page (claimed, so instances starting together share
// one); and decisions read the graph, as they did before the index, until it
// has finished, or until the next tick, at most ten minutes, on an api-only
// process, and for ten minutes more when it had to repair anything.
//
// Anything else reads the graph, which is always right: the index only ever
// lags it (members.go writes in the order that makes that so). Each process
// asks at most every sourceRecheck, says in its log when the answer changes and
// why, and the admin's system checks show the answer.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
)

// SourceEnv is the setting that keeps every decision on the graph ("graph").
const SourceEnv = "CONVERSATION_ACCESS_SOURCE"

// The states the index can be in, as the state row's first field.
const (
	StateClean = "clean" // the last comparison found nothing to repair
	StateDrift = "drift" // the last comparison repaired differences; the next must find none
	StateDirty = "dirty" // a write to the index failed
)

// sourceRecheck is how long one answer is used before the state is read again:
// the longest a process keeps reading the index after another marked it dirty.
var sourceRecheck = 15 * time.Second

// restoredNote is the note of the dirty state make restore leaves
// (Makefile-distribute's RESTORED_INDEX_STATE writes it).
const restoredNote = "restored from a backup"

// started is when this process started, in Unix seconds: a state's stamps are
// to the second (State.String).
var started atomic.Int64

func init() { started.Store(time.Now().Unix()) }

// StartedAt is when this process started, to the second.
func StartedAt() time.Time { return time.Unix(started.Load(), 0) }

// askAhead is how far ahead of this clock another process's start may be for
// the comparison it asked for to be run: one asked for further ahead comes
// from a clock that is wrong, which no comparison here can begin after. That
// process reads the graph, rather than every worker comparing every ten
// minutes for it.
const askAhead = 2 * time.Minute

// Choice is where decisions read, and why, in words an operator acts on.
type Choice struct {
	Index bool
	Why   string
}

// State is the index's state as stored.
type State struct {
	Kind string
	At   time.Time
	Note string
}

func (s State) String() string {
	return strings.Join([]string{s.Kind, s.At.UTC().Format(time.RFC3339), s.Note}, "|")
}

// ParseState reads a stored state; ok is false for anything else. Pure.
func ParseState(v string) (State, bool) {
	parts := strings.SplitN(v, "|", 3)
	if len(parts) < 2 {
		return State{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return State{}, false
	}
	s := State{Kind: parts[0], At: at}
	if len(parts) == 3 {
		s.Note = parts[2]
	}
	switch s.Kind {
	case StateClean, StateDrift, StateDirty:
		return s, true
	}
	return State{}, false
}

// decide is the choice for what is known, in a process that started at start.
// Pure.
func decide(forced string, backfilled bool, state State, haveState bool, start time.Time) Choice {
	switch {
	case strings.EqualFold(strings.TrimSpace(forced), "graph"):
		return Choice{Why: "kept on the graph by " + SourceEnv + "=graph"}
	case !backfilled:
		return Choice{Why: "the conversations index is still being filled in from the graph"}
	case !haveState:
		return Choice{Why: "the conversations index hasn't been compared with the graph yet"}
	}
	at := state.At.UTC().Format(time.RFC3339)
	switch state.Kind {
	case StateClean:
		if staleClean(state, start) {
			return Choice{Why: "the last comparison, at " + at + ", began before this process started at " +
				start.UTC().Format(time.RFC3339) + ", and a database restored from a backup brings back an older comparison's word; " +
				"it reads the graph until a comparison since the start finds the conversations index matches the graph"}
		}
		return Choice{Index: true, Why: "the comparison at " + at + " found the conversations index matches the graph"}
	case StateDrift:
		return Choice{Why: "the comparison at " + at + " repaired differences in the conversations index (" + state.Note + "); it reads the graph until a comparison finds none"}
	default:
		if state.Note == restoredNote {
			return Choice{Why: "the database was restored from a backup at " + at + ", and the conversations index came back older than the graph; it reads the graph until a comparison repairs it"}
		}
		return Choice{Why: "a write to the conversations index failed at " + at + " (" + state.Note + "); it reads the graph until a comparison repairs it"}
	}
}

// staleClean is a clean state whose comparison began before start. Pure.
func staleClean(state State, start time.Time) bool {
	return state.Kind == StateClean && state.At.Before(start)
}

// What is read and written to decide, seams for tests.
var (
	readBackfilled    = func() (bool, error) { return configModel.IsDone(models.BackfillKey) }
	readState         = func(ctx context.Context) (string, bool, error) { return models.ReadState(ctx) }
	readRequest       = models.ComparisonRequested
	requestComparison = models.RequestComparison
)

var (
	sourceMu   sync.Mutex
	sourceLast Choice
	sourceAt   time.Time
	sourceSeen bool
)

// AccessSource is where access decisions read now. A read that fails reads the
// graph (and says why).
func AccessSource(ctx context.Context) Choice {
	sourceMu.Lock()
	defer sourceMu.Unlock()
	if sourceSeen && time.Since(sourceAt) < sourceRecheck {
		return sourceLast
	}
	c := currentChoice(ctx)
	if !sourceSeen || c.Index != sourceLast.Index {
		logFlip(ctx, c)
	}
	sourceLast, sourceAt, sourceSeen = c, time.Now(), true
	return c
}

// logFlip says where decisions read now and why: on a process's first
// decision, and whenever they move between the index and the graph.
var logFlip = func(ctx context.Context, c Choice) {
	where := "the graph"
	if c.Index {
		where = "the conversations index"
	}
	helpers.LogInfoWithContext(ctx, "Conversation access now reads %s: %s", where, c.Why)
}

func currentChoice(ctx context.Context) Choice {
	forced := os.Getenv(SourceEnv)
	if strings.EqualFold(strings.TrimSpace(forced), "graph") {
		return decide(forced, false, State{}, false, time.Time{})
	}
	backfilled, err := readBackfilled()
	if err != nil {
		return Choice{Why: "the conversations index's state couldn't be read: " + err.Error()}
	}
	v, have, err := readState(ctx)
	if err != nil {
		return Choice{Why: "the conversations index's state couldn't be read: " + err.Error()}
	}
	state, ok := ParseState(v)
	start := StartedAt()
	c := decide(forced, backfilled, state, have && ok, start)
	if backfilled && have && ok && staleClean(state, start) {
		askForComparison(ctx, v, start)
	}
	return c
}

var (
	askedMu  sync.Mutex
	askedFor string // the state this process asked a comparison for
)

// askForComparison asks whichever process runs the comparison job for one that
// begins after start (ComparisonDueAfter), once for each state this process
// finds too old. Without it, a process that runs no sweeps would read the graph
// until the clean comparison it found was a day old and compared again.
func askForComparison(ctx context.Context, state string, start time.Time) {
	askedMu.Lock()
	defer askedMu.Unlock()
	if askedFor == state {
		return
	}
	if err := requestComparison(context.WithoutCancel(ctx), start); err != nil {
		// Decisions read the graph meanwhile; the next read of the state asks
		// again.
		helpers.LogErrorWithContext(ctx, "domain/Conversation could not ask for a comparison of the conversations index: %+v", err)
		return
	}
	askedFor = state
}

// ComparisonDueAfter is when a comparison must have begun for every process
// to trust what it found: this process's start, or the latest start another
// process asked for (askForComparison), unless that is askAhead or more ahead
// of this clock.
func ComparisonDueAfter(ctx context.Context) (time.Time, error) {
	after := StartedAt()
	asked, have, err := readRequest(ctx)
	if err != nil {
		return after, err
	}
	if have && asked.After(after) && asked.Before(time.Now().Add(askAhead)) {
		after = asked
	}
	return after, nil
}

// forgetSource makes the next AccessSource read the state again.
func forgetSource() {
	sourceMu.Lock()
	sourceSeen = false
	sourceMu.Unlock()
}

// The state row's writes, seams for tests.
var (
	writeState = models.SetState
	swapState  = models.SwapState
)

// MarkDirty records that a write to the index failed, which sends every
// decision back to the graph until a comparison has repaired it.
func MarkDirty(ctx context.Context, reason string) {
	s := State{Kind: StateDirty, At: time.Now(), Note: oneLine(reason)}
	if err := writeState(context.WithoutCancel(ctx), s.String()); err != nil {
		// The index can't be written and its state can't either: Postgres is
		// away, and every decision that reads the index fails until it's back.
		helpers.LogErrorWithContext(ctx, "domain/Conversation could not mark the conversations index unchecked (%s): %+v", reason, err)
	}
	forgetSource()
}

// Settle records what a comparison that began at began found, unless the state
// changed while it ran (a write failed meanwhile): repaired is how many
// differences it fixed. The state is stamped with when the comparison began,
// which is what a process compares its own start with (decide). It says
// whether the state is now what the comparison found.
func Settle(ctx context.Context, startedFrom string, began time.Time, repaired int, note string) (bool, error) {
	s := State{Kind: StateClean, At: began}
	if repaired > 0 {
		s = State{Kind: StateDrift, At: began, Note: oneLine(fmt.Sprintf("%d repaired: %s", repaired, note))}
	}
	ok, err := swapState(ctx, startedFrom, s.String())
	forgetSource()
	return ok, err
}

// StateNow is the stored state as it is, for a comparison to start from.
func StateNow(ctx context.Context) (string, error) {
	v, _, err := models.ReadState(ctx)
	return v, err
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// The admin's system checks say where decisions read, and why.
func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "conversation-access",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Says whether access to channels, DMs and groups is decided from the conversations index in " +
			"Postgres or from the graph, and why. It reads the index's state; it doesn't compare the two itself.",
		Probe: func(ctx context.Context) error {
			c := currentChoice(ctx)
			if c.Index {
				return nil
			}
			if strings.Contains(c.Why, "couldn't be read") {
				return errors.New(c.Why)
			}
			return helpers.SystemCheckNote("Decided from the graph: " + c.Why + ".")
		},
	})
}
