package business

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// Notes are drafted from what the asker may read of the release: its live
// tasks of the project they may see, each with its pull requests' titles;
// a restricted, deleted or moved task never reaches the drafter. Without a
// drafter (the AI-free edition) there are no drafts: not found.
func TestNotesAreDraftedFromWhatTheAskerMaySee(t *testing.T) {
	w := newWorld(t, workAccess.RoleAdmin)
	was := prTitles
	t.Cleanup(func() { prTitles = was; RegisterNotesDrafter(nil) })
	var asked []uuid.UUID
	prTitles = func(_ context.Context, tasks []uuid.UUID) (map[uuid.UUID][]string, error) {
		asked = tasks
		return map[uuid.UUID][]string{w.a: {"Checkout totals in cents"}, w.c: {"Secret work"}}, nil
	}

	RegisterNotesDrafter(nil)
	if DraftsNotes() {
		t.Fatal("drafts notes with no drafter")
	}
	if _, err := DraftNotes(context.Background(), ben, w.rel); !errors.Is(err, helpers.ErrNotFound) {
		t.Fatalf("no drafter: %v, want not found", err)
	}

	var brief NotesBrief
	var asker string
	RegisterNotesDrafter(func(_ context.Context, who string, b NotesBrief) (string, error) {
		asker, brief = who, b
		return "### New\n- Totals in cents", nil
	})
	notes, err := DraftNotes(context.Background(), ben, w.rel)
	if err != nil || !strings.Contains(notes, "Totals in cents") || asker != ben.UserUUID {
		t.Fatalf("draft %q %v as %q", notes, err, asker)
	}
	if brief.Release != "v1.0" || len(brief.Tasks) != 2 || brief.Tasks[0].Name != "task "+w.a.String()[:4] ||
		strings.Join(brief.Tasks[0].PRs, ",") != "Checkout totals in cents" || brief.Tasks[1].Status != "done" {
		t.Errorf("brief %+v", brief)
	}
	if len(asked) != 2 || asked[0] != w.a || asked[1] != w.b {
		t.Errorf("titles read for %v, want only the tasks the asker sees", asked)
	}
}

type recorder struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (r *recorder) Emit(_ context.Context, e resourceEventBusiness.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// A build's or deploy's report is kept in bounds and tells each release whose
// stage it changed (release.stage), as the integration's bot; a kind other
// than build or deploy, or a stage without a name, is refused.
func TestAStageReportTellsTheReleasesItChanged(t *testing.T) {
	was, bus := keepStage, resourceEventBusiness.Bus
	rec := &recorder{}
	t.Cleanup(func() { keepStage, resourceEventBusiness.Bus = was, bus })
	resourceEventBusiness.Bus = rec
	one, two := uuid.New(), uuid.New()
	var kept releaseModel.Stage
	keepStage = func(_ context.Context, projects []uuid.UUID, ref string, s releaseModel.Stage) ([]uuid.UUID, error) {
		kept = s
		return []uuid.UUID{one, two}, nil
	}
	bot := uuid.NewString()
	s := releaseModel.Stage{Provider: "github", Repo: "Acme/Shop", Kind: "deploy", Name: strings.Repeat("e", 300),
		Status: "SUCCESS", SHA: "abc", URL: "https://github.com/acme/shop/actions/runs/1", ReportedAt: time.Now()}
	if err := ReportStage(context.Background(), []uuid.UUID{uuid.New()}, "v1.1", s, bot); err != nil {
		t.Fatal(err)
	}
	if kept.Repo != "acme/shop" || len([]rune(kept.Name)) != 200 || kept.Status != "success" {
		t.Errorf("kept %+v", kept)
	}
	if len(rec.events) != 2 || rec.events[0].Kind != resourceEventBusiness.ReleaseStage || rec.events[0].ResourceKind != "release" ||
		rec.events[0].ResourceUUID != one.String() || rec.events[0].Actor.UserUUID != bot || rec.events[0].Data["status"] != "success" ||
		rec.events[0].Data["kind"] != "deploy" || rec.events[1].ResourceUUID != two.String() {
		t.Errorf("events %+v", rec.events)
	}
	for _, bad := range []releaseModel.Stage{{Provider: "github", Repo: "acme/shop", Kind: "test", Name: "CI", Status: "ok"},
		{Provider: "github", Repo: "acme/shop", Kind: "build", Name: " ", Status: "ok"}} {
		if err := ReportStage(context.Background(), nil, "v1", bad, bot); err == nil {
			t.Errorf("%+v kept", bad)
		}
	}
}

// A tag names its release on one line, in a release name's bounds.
func TestATagNamesItsRelease(t *testing.T) {
	if got := tagName("  v2.1\n"); got != "v2.1" {
		t.Errorf("tagName = %q", got)
	}
	if got := tagName(strings.Repeat("é", 200)); len([]rune(got)) != nameMost {
		t.Errorf("a long tag's name has %d characters", len([]rune(got)))
	}
}

// A tag's tasks go in as git through the one put-in (addRows), capped at
// MostTasks. A release full, or no longer live, takes none and says so
// (ReleaseError: shipTask ships it without them, the delivery done); a
// failed write is returned, for the delivery to come again.
func TestATagsTasksGoInAsGitOrSayWhyNot(t *testing.T) {
	was := addRows
	t.Cleanup(func() { addRows = was })
	rel, bot := uuid.New(), uuid.New()
	var got []uuid.UUID
	var source string
	answer := error(nil)
	addRows = func(_ context.Context, _ uuid.UUID, tasks []uuid.UUID, s string, _ uuid.UUID, _ int) ([]string, error) {
		got, source = tasks, s
		return nil, answer
	}
	many := make([]uuid.UUID, MostTasks+3)
	if err := addFromGit(context.Background(), rel, many, bot); err != nil || len(got) != MostTasks || source != releaseModel.Git {
		t.Errorf("put in %d as %q (%v), want %d as git", len(got), source, err, MostTasks)
	}
	for _, refused := range []error{releaseModel.ErrFull, helpers.ErrNotFound} {
		answer = refused
		var re *ReleaseError
		if err := addFromGit(context.Background(), rel, many[:1], bot); !errors.As(err, &re) {
			t.Errorf("%v: %v, want a ReleaseError", refused, err)
		}
	}
	answer = errors.New("connection reset")
	if err := addFromGit(context.Background(), rel, many[:1], bot); err == nil || errors.As(err, new(*ReleaseError)) {
		t.Errorf("a failed write: %v, want it returned to be delivered again", err)
	}
}

// tagBook fakes the releases a tag ships, by project (a release's id is its
// project's), and inTx's all-or-nothing: a failed transaction's writes are
// undone. shipping answers a stale previous tag once where stale says.
type tagBook struct {
	since, stale map[uuid.UUID]string
	shipped      map[uuid.UUID]bool
	tasks        map[uuid.UUID][]uuid.UUID
	addFails     map[uuid.UUID]error // once, but ErrFull every time
	reads        map[string]int      // named's reads, by previous tag
	race         map[uuid.UUID]bool  // once: the twin delivery ships right after shipping's read
}

func newTagBook(t *testing.T, since map[uuid.UUID]string) *tagBook {
	b := &tagBook{since: since, stale: map[uuid.UUID]string{}, shipped: map[uuid.UUID]bool{},
		tasks: map[uuid.UUID][]uuid.UUID{}, addFails: map[uuid.UUID]error{}, reads: map[string]int{}, race: map[uuid.UUID]bool{}}
	was := []any{shipping, shipRow, addRows, inTx, helpers.MessageLogs}
	t.Cleanup(func() {
		shipping = was[0].(func(context.Context, uuid.UUID, releaseModel.Tag) (releaseModel.Shipped, error))
		shipRow = was[1].(func(context.Context, uuid.UUID, releaseModel.Tag, uuid.UUID) (releaseModel.Shipped, error))
		addRows = was[2].(func(context.Context, uuid.UUID, []uuid.UUID, string, uuid.UUID, int) ([]string, error))
		inTx = was[3].(func(context.Context, func(context.Context) error) error)
		helpers.MessageLogs = was[4].(*helpers.Message)
	})
	discard := log.New(io.Discard, "", 0)
	helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	state := func(project uuid.UUID) releaseModel.Shipped {
		return releaseModel.Shipped{Release: releaseModel.Release{ID: project}, Since: b.since[project], Again: b.shipped[project]}
	}
	shipping = func(_ context.Context, project uuid.UUID, _ releaseModel.Tag) (releaseModel.Shipped, error) {
		s := state(project)
		if stale, ok := b.stale[project]; ok {
			delete(b.stale, project)
			s.Since = stale
		}
		if b.race[project] {
			delete(b.race, project)
			b.shipped[project] = true
		}
		return s, nil
	}
	shipRow = func(_ context.Context, project uuid.UUID, _ releaseModel.Tag, _ uuid.UUID) (releaseModel.Shipped, error) {
		s := state(project)
		b.shipped[project] = true
		return s, nil
	}
	addRows = func(_ context.Context, release uuid.UUID, tasks []uuid.UUID, _ string, _ uuid.UUID, _ int) ([]string, error) {
		if err := b.addFails[release]; err != nil {
			if !errors.Is(err, releaseModel.ErrFull) {
				delete(b.addFails, release)
			}
			return nil, err
		}
		b.tasks[release] = append(b.tasks[release], tasks...)
		return nil, nil
	}
	inTx = func(ctx context.Context, fn func(context.Context) error) error {
		shipped, tasks := maps.Clone(b.shipped), maps.Clone(b.tasks)
		err := fn(ctx)
		if err != nil {
			b.shipped, b.tasks = shipped, tasks
		}
		return err
	}
	return b
}

// named reads a tag's tasks by previous tag from tasks, failing the first
// read of a previous tag fail names.
func (b *tagBook) named(tasks map[string]map[uuid.UUID][]uuid.UUID, fail string) TasksSince {
	return func(_ context.Context, since string) (map[uuid.UUID][]uuid.UUID, error) {
		b.reads[since]++
		if since == fail && b.reads[since] == 1 {
			return nil, errors.New("github API error 502")
		}
		return tasks[since], nil
	}
}

// A tag's tasks are read before any write, and each project's release ships
// with them in one transaction: a failed read or write ships nothing there,
// so the delivery that comes again puts them in, while a project that
// shipped before the failure keeps its release WITH its tasks, and isn't
// read or put in again (a task a person took out since stays out).
func TestATagShipsEachReleaseWithItsTasksOrNotAtAll(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	a1, b1, c1 := uuid.New(), uuid.New(), uuid.New()
	book := newTagBook(t, map[uuid.UUID]string{a: "v1.0", b: "v0.9", c: "v1.0"})
	named := book.named(map[string]map[uuid.UUID][]uuid.UUID{"v1.0": {a: {a1}, c: {c1}}, "v0.9": {b: {b1}}}, "v0.9")
	book.addFails[c] = errors.New("connection reset")
	ctx, bot, projects := context.Background(), uuid.New(), []uuid.UUID{a, b, c}
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C"}
	want := func(when string, shipped map[uuid.UUID][]uuid.UUID) {
		t.Helper()
		for _, p := range projects {
			tasks, ok := shipped[p]
			if book.shipped[p] != ok || fmt.Sprint(book.tasks[p]) != fmt.Sprint(tasks) {
				t.Errorf("%s: %s shipped %v with %d tasks, want %v with %d", when, names[p], book.shipped[p], len(book.tasks[p]), ok, len(tasks))
			}
		}
	}
	// B's compare fails: A shipped with its task before it, B ships nothing.
	if err := ShipTag(ctx, projects, "acme/shop", "v1.1", "sha", named, bot); err == nil {
		t.Errorf("a failed read was the delivery's success")
	}
	want("B's read failed", map[uuid.UUID][]uuid.UUID{a: {a1}})
	// Delivered again: B ships with its task; C's write fails, shipping nothing.
	if err := ShipTag(ctx, projects, "acme/shop", "v1.1", "sha", named, bot); err == nil {
		t.Errorf("a failed write was the delivery's success")
	}
	want("C's write failed", map[uuid.UUID][]uuid.UUID{a: {a1}, b: {b1}})
	// Again: C ships with its task; A and B, shipped, are neither read nor put in again.
	if err := ShipTag(ctx, projects, "acme/shop", "v1.1", "sha", named, bot); err != nil {
		t.Errorf("third delivery: %v", err)
	}
	want("delivered a third time", map[uuid.UUID][]uuid.UUID{a: {a1}, b: {b1}, c: {c1}})
	// A person takes A's task out; the tag's twin event leaves it out, reading nothing.
	book.tasks[a] = nil
	reads := maps.Clone(book.reads)
	if err := ShipTag(ctx, projects, "acme/shop", "v1.1", "sha", named, bot); err != nil || !maps.Equal(reads, book.reads) || len(book.tasks[a]) != 0 {
		t.Errorf("the twin: %v, reads %v then %v, A's tasks %v: want no read and A's task left out", err, reads, book.reads, book.tasks[a])
	}
}

// A tag's create and its release published arrive at once (GitHub sends
// both): each read sees the release unshipped, and the one that ships second
// finds it shipped under the lock. It puts nothing in, so a task a person took
// out of the release since the first ship stays out.
func TestATwinThatShipsSecondPutsNothingIn(t *testing.T) {
	a, a1 := uuid.New(), uuid.New()
	book := newTagBook(t, map[uuid.UUID]string{a: "v1.0"})
	named := book.named(map[string]map[uuid.UUID][]uuid.UUID{"v1.0": {a: {a1}}}, "")
	book.race[a] = true
	if err := ShipTag(context.Background(), []uuid.UUID{a}, "acme/shop", "v1.1", "sha", named, uuid.New()); err != nil {
		t.Errorf("the twin that shipped second: %v, want nil", err)
	}
	if !book.shipped[a] || len(book.tasks[a]) != 0 {
		t.Errorf("the twin that shipped second: shipped %v with %v, want shipped and nothing put in", book.shipped[a], book.tasks[a])
	}
}

// A release that can't take a tag's tasks (full) ships without them, and a
// previous tag shipped between the read of the tasks and the ship has them
// read again, from it.
func TestATagShipsAFullReleaseBareAndRereadsAMovedPreviousTag(t *testing.T) {
	full, moved := uuid.New(), uuid.New()
	f1, m1, m2 := uuid.New(), uuid.New(), uuid.New()
	book := newTagBook(t, map[uuid.UUID]string{full: "v1.0", moved: "v1.05"})
	book.addFails[full] = releaseModel.ErrFull
	book.stale[moved] = "v1.0"
	named := book.named(map[string]map[uuid.UUID][]uuid.UUID{"v1.0": {full: {f1}, moved: {m1}}, "v1.05": {moved: {m2}}}, "")
	if err := ShipTag(context.Background(), []uuid.UUID{full, moved}, "acme/shop", "v1.1", "sha", named, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if !book.shipped[full] || len(book.tasks[full]) != 0 {
		t.Errorf("the full release shipped %v with %v, want shipped bare", book.shipped[full], book.tasks[full])
	}
	if !book.shipped[moved] || fmt.Sprint(book.tasks[moved]) != fmt.Sprint([]uuid.UUID{m2}) || book.reads["v1.05"] != 1 {
		t.Errorf("the moved release shipped %v with %v (reads %v), want the tasks since v1.05", book.shipped[moved], book.tasks[moved], book.reads)
	}
}
