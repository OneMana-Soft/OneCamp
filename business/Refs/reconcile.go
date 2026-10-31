package refsBusiness

// The nightly reconciler and the rebuild: refers_to re-derived from the
// bodies, in pages, the drift between what the graph holds and what the
// bodies say reported per kind and fixed.
//
// WHAT IT CATCHES. An event that was never written (Postgres down at the
// save), a sink that gave up after its last try, a writer that doesn't tell
// this package (an import, a restore from backup, a body changed in the graph
// by hand), and an edge left to a target since removed. It counts as drift
// only what it would change: a link to a uuid that names nothing is no edge
// and no drift.
//
// WHAT IT COSTS, bounded. Each night reads at most NightlyPerKind sources of
// each kind, in pages (PageSize; DocPageSize for docs, whose bodies are
// long), each page one graph read, at most one existence read and at most one
// write, with a pause between pages, and stops at NightlyBudget whatever is
// left. It goes on the next night where it stopped (WorkGraphRefsCursor), so
// a large workspace is covered over several nights, and starts again at the
// beginning once it reaches the end. It also re-reads every doc that holds a
// to-do for a task (doc_task_links: the mirror), at most MirrorPerNight.
// The rebuild (`go-one-camp workgraphrefs [--apply]`) has no bound and no
// cursor: every source of every kind, from the beginning.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	linkModels "github.com/akashc777/OneCamp/models/postgres/EntityLink"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

// The reconciler's bounds.
const (
	PageSize       = 500
	DocPageSize    = 50
	NightlyPerKind = 20000
	MirrorPerNight = 5000
	NightlyBudget  = 20 * time.Minute
	nightlyPause   = 50 * time.Millisecond
	// fixesPerWrite bounds one write's request.
	fixesPerWrite = 100
)

// The kinds whose bodies a pass reads, in its order; the projected kinds
// (passNodes) follow them.
var passKinds = []domain.Kind{Message, Chat, Comment, Doc, Task}

// Report is one kind's pass: what it read and the drift it found (and fixed,
// when it applied).
type Report struct {
	Kind    domain.Kind `json:"kind"`
	Read    int         `json:"read"`
	Drifted int         `json:"drifted"`
	Added   int         `json:"added"`
	Removed int         `json:"removed"`
	// Wrapped is a pass that reached the end of the kind's sources (the next
	// night starts at the beginning).
	Wrapped bool `json:"wrapped"`
	Applied bool `json:"applied"`
}

func (r Report) String() string {
	verb := "would add"
	if r.Applied {
		verb = "added"
	}
	return fmt.Sprintf("%s: read %d, %d drifted, %s %d edges and took off %d", r.Kind, r.Read, r.Drifted, verb, r.Added, r.Removed)
}

// Options are a pass's bounds.
type Options struct {
	Apply   bool
	PerKind int // 0: no bound
	Budget  time.Duration
	Pause   time.Duration
	Kinds   []domain.Kind
	// Cursor reads and keeps where each kind's pass stopped; nil starts at the
	// beginning and keeps nothing.
	Cursor Cursor
	// Mirror is how many docs that hold a to-do for a task are re-read first;
	// 0 none, -1 all.
	Mirror int
}

// Cursor keeps where a kind's pass stopped.
type Cursor interface {
	Load(ctx context.Context, k domain.Kind) string
	Save(ctx context.Context, k domain.Kind, after string)
}

// Seams for tests.
var (
	readPage       = domain.ReadSources
	readByUUID     = domain.ReadSourcesByUUID
	existing       = domain.Existing
	docsWithTasks  = linkModels.DocsWithTasks
	reconcileClock = time.Now
)

// ReconcileAll is one pass over every kind (opts.Kinds, or all): the mirror
// of doc_task_links first, then each kind's sources in pages, then each
// projected kind's nodes. A page that fails stops its kind's pass (the cursor
// stays before it) and the next kind goes on; the error is the first one.
func ReconcileAll(ctx context.Context, opts Options) ([]Report, error) {
	start := reconcileClock()
	over := func() bool {
		return ctx.Err() != nil || (opts.Budget > 0 && reconcileClock().Sub(start) >= opts.Budget)
	}
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = append(append([]domain.Kind(nil), passKinds...), projectedKinds()...)
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	var out []Report
	if opts.Mirror != 0 {
		r, err := mirrorDocTasks(ctx, opts, over)
		keep(err)
		out = append(out, r)
	}
	for _, k := range kinds {
		run := passKind
		if domain.IsProjected(k) {
			run = passNodes
		}
		r, err := run(ctx, k, opts, over)
		keep(err)
		out = append(out, r)
		if over() {
			break
		}
	}
	return out, firstErr
}

// passKind reads k's sources from its cursor, page by page, within bounds.
func passKind(ctx context.Context, k domain.Kind, opts Options, over func() bool) (Report, error) {
	rep := Report{Kind: k, Applied: opts.Apply}
	size := PageSize
	if k == Doc {
		size = DocPageSize
	}
	err := pass(ctx, k, opts, over, size, &rep, func(after string, n int) (string, error) {
		page, err := readPage(ctx, k, after, n)
		if err != nil {
			return "", err
		}
		if err := fixPage(ctx, k, page, opts.Apply, &rep); err != nil {
			return "", err
		}
		if len(page) < n {
			return "", nil
		}
		return page[len(page)-1].UID, nil
	})
	return rep, err
}

// pass is one kind's pages from its cursor, within opts' bounds and the
// clock: page reads and fixes at most n after the cursor and answers the
// cursor after them ("" once it reached the end, so the next pass starts
// again from the beginning). A page that fails leaves the cursor before it.
func pass(ctx context.Context, k domain.Kind, opts Options, over func() bool, size int, rep *Report, page func(after string, n int) (string, error)) error {
	after := ""
	if opts.Cursor != nil {
		after = opts.Cursor.Load(ctx, k)
	}
	for !over() && (opts.PerKind <= 0 || rep.Read < opts.PerKind) {
		n := size
		if opts.PerKind > 0 && opts.PerKind-rep.Read < n {
			n = opts.PerKind - rep.Read
		}
		next, err := page(after, n)
		if err != nil {
			return err
		}
		after, rep.Wrapped = next, next == ""
		if opts.Cursor != nil {
			opts.Cursor.Save(ctx, k, after)
		}
		if rep.Wrapped {
			break
		}
		if opts.Pause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(opts.Pause):
			}
		}
	}
	return nil
}

// mirrorDocTasks re-reads the docs that hold a to-do for a task, so each
// one's refers_to holds its to-dos' tasks: doc_task_links mirrored.
func mirrorDocTasks(ctx context.Context, opts Options, over func() bool) (Report, error) {
	rep := Report{Kind: "doc_task_links", Applied: opts.Apply}
	after := uuid.Nil
	for !over() && (opts.Mirror < 0 || rep.Read < opts.Mirror) {
		docs, err := docsWithTasks(ctx, after, DocPageSize)
		if err != nil || len(docs) == 0 {
			if err == nil {
				rep.Wrapped = true
			}
			return rep, err
		}
		after = docs[len(docs)-1]
		ids := make([]string, 0, len(docs))
		for _, d := range docs {
			ids = append(ids, d.String())
		}
		page, err := readByUUID(ctx, Doc, ids)
		if err != nil {
			return rep, err
		}
		if err := fixPage(ctx, Doc, page, opts.Apply, &rep); err != nil {
			return rep, err
		}
		if len(docs) < DocPageSize {
			rep.Wrapped = true
			return rep, nil
		}
	}
	return rep, nil
}

// fixPage finds a page's drift and, when apply, fixes it: one existence read
// for what it would add, one write per fixesPerWrite sources that changed.
func fixPage(ctx context.Context, k domain.Kind, page []domain.Source, apply bool, rep *Report) error {
	rep.Read += len(page)
	if len(page) == 0 {
		return nil
	}
	var tasks map[string][]string
	if k == Doc {
		ids := make([]string, 0, len(page))
		for _, s := range page {
			ids = append(ids, s.UUID)
		}
		var err error
		if tasks, err = docTasks(ctx, ids); err != nil {
			return err
		}
	}
	fixes := make([]domain.Fix, 0, len(page))
	var adds []domain.Ref
	for _, s := range page {
		f := domain.Diff(s, Want(s, tasks[s.UUID]))
		if !f.Empty() {
			fixes = append(fixes, f)
			adds = append(adds, f.Add...)
		}
	}
	if len(adds) > 0 {
		// A link to nothing adds no edge: it isn't drift.
		there, err := existing(ctx, adds)
		if err != nil {
			return err
		}
		kept := fixes[:0]
		for _, f := range fixes {
			add := f.Add[:0]
			for _, r := range f.Add {
				if there[r] {
					add = append(add, r)
				}
			}
			f.Add = add
			if !f.Empty() {
				kept = append(kept, f)
			}
		}
		fixes = kept
	}
	for _, f := range fixes {
		rep.Drifted++
		rep.Added += len(f.Add)
		rep.Removed += len(f.Del)
	}
	if !apply {
		return nil
	}
	for from := 0; from < len(fixes); from += fixesPerWrite {
		if err := writeFixes(ctx, fixes[from:min(from+fixesPerWrite, len(fixes))]); err != nil {
			return err
		}
	}
	return nil
}

// redisCursor keeps each kind's cursor in Redis, and in this process when
// Redis isn't there, so a single instance still goes on where it stopped.
type redisCursor struct {
	mu    sync.Mutex
	local map[domain.Kind]string
}

func (c *redisCursor) Load(ctx context.Context, k domain.Kind) string {
	if rdb := redisInit.RedisClient; rdb != nil {
		// A body kind's cursor is a graph uid; a projected kind's is its
		// projector's own.
		if v, err := rdb.Get(ctx, registry.WorkGraphRefsCursor.Build(string(k))).Result(); err == nil && (domain.IsProjected(k) || helpers.IsDgraphUID(v)) {
			return v
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local[k]
}

func (c *redisCursor) Save(ctx context.Context, k domain.Kind, after string) {
	c.mu.Lock()
	if c.local == nil {
		c.local = map[domain.Kind]string{}
	}
	c.local[k] = after
	c.mu.Unlock()
	if rdb := redisInit.RedisClient; rdb != nil {
		if err := rdb.Set(ctx, registry.WorkGraphRefsCursor.Build(string(k)), after, registry.WorkGraphRefsCursor.TTL).Err(); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Refs: where the %s pass stopped wasn't kept, so the next starts again: %v", k, err)
		}
	}
}

var nightlyCursor = &redisCursor{}

// lastReport is this process's last nightly report, for the system check
// when Redis isn't there.
var lastReport struct {
	sync.Mutex
	at      time.Time
	reports []Report
	err     string
}

// nightly is the Sweeper's job.
func nightly(ctx context.Context) error {
	reports, err := ReconcileAll(ctx, Options{
		Apply: true, PerKind: NightlyPerKind, Budget: NightlyBudget, Pause: nightlyPause,
		Cursor: nightlyCursor, Mirror: MirrorPerNight,
	})
	keepReport(ctx, reports, err)
	for _, r := range reports {
		if r.Drifted > 0 {
			helpers.LogInfoWithContext(ctx, "business/Refs nightly reconcile: %s", r)
		}
	}
	return err
}

type storedReport struct {
	At      time.Time `json:"at"`
	Reports []Report  `json:"reports"`
	Err     string    `json:"error,omitempty"`
}

func keepReport(ctx context.Context, reports []Report, err error) {
	s := storedReport{At: reconcileClock().UTC(), Reports: reports}
	if err != nil {
		s.Err = err.Error()
	}
	lastReport.Lock()
	lastReport.at, lastReport.reports, lastReport.err = s.At, reports, s.Err
	lastReport.Unlock()
	if rdb := redisInit.RedisClient; rdb != nil {
		if b, jerr := json.Marshal(s); jerr == nil {
			_ = rdb.Set(ctx, registry.WorkGraphRefsReport.Build(), b, registry.WorkGraphRefsReport.TTL).Err()
		}
	}
}

// readReport is the last nightly report: Redis's, else this process's.
func readReport(ctx context.Context) (storedReport, bool) {
	if rdb := redisInit.RedisClient; rdb != nil {
		if b, err := rdb.Get(ctx, registry.WorkGraphRefsReport.Build()).Bytes(); err == nil {
			var s storedReport
			if json.Unmarshal(b, &s) == nil {
				return s, true
			}
		}
	}
	lastReport.Lock()
	defer lastReport.Unlock()
	if lastReport.at.IsZero() {
		return storedReport{}, false
	}
	return storedReport{At: lastReport.at, Reports: lastReport.reports, Err: lastReport.err}, true
}

// probe is the admins' system check: the last night's drift.
func probe(ctx context.Context) error {
	s, ok := readReport(ctx)
	if !ok {
		return helpers.SystemCheckNote("The work graph's links haven't been reconciled since this server started; the first pass runs within a day.")
	}
	var parts []string
	drift := 0
	for _, r := range s.Reports {
		drift += r.Drifted
		parts = append(parts, r.String())
	}
	if s.Err != "" {
		return fmt.Errorf("the last reconcile of the work graph's links (%s) stopped: %s", s.At.Format(time.RFC3339), s.Err)
	}
	note := fmt.Sprintf("Last reconciled %s: %s.", s.At.Format(time.RFC3339), strings.Join(parts, "; "))
	if drift > 0 {
		note += " Drift was fixed; a recurring count means a writer isn't telling the work graph of its saves."
	}
	return helpers.SystemCheckNote(note)
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "workgraph.refs.reconcile", Every: 24 * time.Hour, Run: nightly})
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "work-graph-links",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Reads the last nightly reconcile of the work graph's links (what messages, comments and docs link to) " +
			"and of its meetings' nodes: how many it read and the drift it found and fixed. It reads nothing itself.",
		Probe: probe,
	})
}
