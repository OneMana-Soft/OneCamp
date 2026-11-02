//go:build integration

package models_test

// V-179 (the owner's rule: every delete is soft) against Postgres 12 with
// every migration. Taking a person off a restricted task, lifting a
// restriction and a person's leaving its project keep the rows, marked with
// when and by whom, and every read takes live rows only. Restricting the task
// again, or letting the person in again, brings the same row back.
//
// "Zephyr pay review" is in the Website project; Maya restricts it, with
// "Draft the Zephyr letters" below it. Ivy and Hal are let in; Ben is not.
// Ana, another of the project's admins, acts after Maya, so a test can tell a
// mark (or a restriction, or a grant) Maya made from one Ana made later.
// Run: PKGS=./models/postgres/TaskRestriction/ integ_rel.sh <wt> <log> '^TestV179'

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	restriction "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

type world struct {
	db                       *sql.DB
	project                  uuid.UUID
	maya, ivy, hal, ben, ana uuid.UUID
	secret, below, beside    uuid.UUID
}

func setup(t *testing.T) (context.Context, world) {
	t.Helper()
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	w := world{db: env.PG, project: uuid.New(), maya: uuid.New(), ivy: uuid.New(), hal: uuid.New(), ben: uuid.New(), ana: uuid.New(),
		secret: uuid.New(), below: uuid.New(), beside: uuid.New()}
	for name, id := range map[string]uuid.UUID{"maya": w.maya, "ivy": w.ivy, "hal": w.hal, "ben": w.ben, "ana": w.ana} {
		w.exec(t, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"-"+id.String()[:8]+"@example.test", name, name)
	}
	w.exec(t, `INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, w.project, w.maya)
	for _, id := range []uuid.UUID{w.secret, w.below, w.beside} {
		w.exec(t, `INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, w.project, w.maya)
	}
	return ctx, w
}

func (w world) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := w.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// count is how many rows q counts.
func (w world) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// mark is a kept row's: when it was taken off (not valid while it lives) and by whom.
func (w world) mark(t *testing.T, q string, args ...any) (sql.NullTime, uuid.NullUUID) {
	t.Helper()
	var (
		at sql.NullTime
		by uuid.NullUUID
	)
	if err := w.db.QueryRow(q, args...).Scan(&at, &by); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return at, by
}

const (
	grantRows       = `SELECT count(*) FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`
	liveGrantRows   = `SELECT count(*) FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2 AND deleted_at IS NULL`
	grantMark       = `SELECT deleted_at, deleted_by FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`
	restrictionRows = `SELECT count(*) FROM task_restrictions WHERE task_id = $1`
	restrictionMark = `SELECT deleted_at, deleted_by FROM task_restrictions WHERE task_id = $1`
)

// letIn is whether the rule's read lets who into task, which must be a candidate.
func letIn(t *testing.T, ctx context.Context, who, task uuid.UUID) bool {
	t.Helper()
	cs, err := restriction.Candidates(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.TaskID == task {
			return c.LetIn
		}
	}
	t.Fatalf("%s is no candidate for %s", task, who)
	return false
}

func grantsOf(t *testing.T, ctx context.Context, task uuid.UUID) []uuid.UUID {
	t.Helper()
	r, err := restriction.Of(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	return r.Grants
}

func TestV179TakingSomeoneOffARestrictedTaskKeepsTheirGrantMarked(t *testing.T) {
	ctx, w := setup(t)
	first := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, []uuid.UUID{w.below}, first); err != nil {
		t.Fatal(err)
	}
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, nil, first.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Hal's row stays, marked with who took him off and when.
	if n := w.count(t, grantRows, w.secret, w.hal); n != 1 {
		t.Fatalf("Hal's grant: %d rows, want his own kept (every delete is soft)", n)
	}
	if at, by := w.mark(t, grantMark, w.secret, w.hal); !at.Valid || by.UUID != w.maya {
		t.Fatalf("Hal's grant is marked %v by %v, want taken off by Maya", at, by)
	}
	// Ivy, let in both times, keeps the grant she had, from when it was made.
	var granted time.Time
	if err := w.db.QueryRow(`SELECT granted_at FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, w.secret, w.ivy).Scan(&granted); err != nil {
		t.Fatal(err)
	}
	if !granted.Equal(first) {
		t.Fatalf("Ivy's grant is from %v, want %v: a grant kept is not made again", granted, first)
	}
	if got := grantsOf(t, ctx, w.secret); !slices.Equal(got, []uuid.UUID{w.ivy}) {
		t.Fatalf("grants %v, want Ivy's alone", got)
	}
	if letIn(t, ctx, w.hal, w.secret) || !letIn(t, ctx, w.ivy, w.secret) || !letIn(t, ctx, w.ivy, w.below) {
		t.Fatal("the rule still lets Hal in, or no longer lets Ivy into the task or the one below it")
	}
	// Let in again, Hal's own row comes back to life.
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, nil, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, live := w.count(t, grantRows, w.secret, w.hal), w.count(t, liveGrantRows, w.secret, w.hal); n != 1 || live != 1 {
		t.Fatalf("Hal let in again: %d rows, %d live, want his one row live", n, live)
	}
	if !letIn(t, ctx, w.hal, w.secret) {
		t.Fatal("Hal let in again isn't let in")
	}
}

func TestV179LiftingARestrictionKeepsItsRowsMarked(t *testing.T) {
	ctx, w := setup(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, []uuid.UUID{w.below}, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := restriction.Unrestrict(ctx, w.secret, w.maya); err != nil || !changed {
		t.Fatalf("lifting it: changed %v, %v", changed, err)
	}
	// Its row and the row of the task restricted with it stay, marked; so does Ivy's grant.
	for _, id := range []uuid.UUID{w.secret, w.below} {
		if n := w.count(t, restrictionRows, id); n != 1 {
			t.Fatalf("%s's restriction: %d rows, want it kept (every delete is soft)", id, n)
		}
		if at, by := w.mark(t, restrictionMark, id); !at.Valid || by.UUID != w.maya {
			t.Fatalf("%s's restriction is marked %v by %v, want lifted by Maya", id, at, by)
		}
	}
	if n := w.count(t, grantRows, w.secret, w.ivy); n != 1 {
		t.Fatalf("Ivy's grant: %d rows, want it kept", n)
	}
	if at, by := w.mark(t, grantMark, w.secret, w.ivy); !at.Valid || by.UUID != w.maya {
		t.Fatalf("Ivy's grant is marked %v by %v after the restriction was lifted, want by Maya", at, by)
	}
	// Every read takes live rows only: the tasks are open to the project again.
	for _, id := range []uuid.UUID{w.secret, w.below} {
		if r, err := restriction.Of(ctx, id); err != nil || r.Restricted {
			t.Fatalf("%s reads as restricted (%v) after it was lifted", id, err)
		}
	}
	cs, err := restriction.Candidates(ctx, w.ben)
	if err != nil {
		t.Fatal(err)
	}
	among, err := restriction.CandidatesAmong(ctx, []uuid.UUID{w.ben})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(cs, among[w.ben]...) {
		if c.TaskID == w.secret || c.TaskID == w.below {
			t.Fatalf("%s is still a restricted task for the rule", c.TaskID)
		}
	}
	// Lifted, it isn't lifted again, and a task made below it isn't restricted with it.
	if again, err := restriction.Unrestrict(ctx, w.secret, w.maya); err != nil || again {
		t.Fatalf("lifting it again: changed %v, %v", again, err)
	}
	if err := restriction.Inherit(ctx, w.beside, w.secret, w.maya, now); err != nil {
		t.Fatal(err)
	}
	if r, err := restriction.Of(ctx, w.beside); err != nil || r.Restricted {
		t.Fatalf("a task made below a lifted one is restricted with it (%v)", err)
	}
	// Restricted again, its own rows come back, with whom it's kept to now.
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.hal}, []uuid.UUID{w.below}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, err := restriction.Of(ctx, w.secret)
	if err != nil || !r.Restricted || r.RestrictedBy != w.maya || !slices.Equal(r.Grants, []uuid.UUID{w.hal}) {
		t.Fatalf("restricted again: %+v (%v), want restricted by Maya to Hal", r, err)
	}
	if b, err := restriction.Of(ctx, w.below); err != nil || !b.Restricted || b.Via != w.secret {
		t.Fatalf("the task below, restricted again: %+v (%v), want restricted with it", b, err)
	}
	if n := w.count(t, restrictionRows, w.secret); n != 1 {
		t.Fatalf("restricted again: %d rows, want its own one", n)
	}
	if live := w.count(t, liveGrantRows, w.secret, w.ivy); live != 0 {
		t.Fatal("Ivy, not let in this time, is let in again")
	}
}

func TestV179LeavingAProjectKeepsTheirGrantsMarked(t *testing.T) {
	ctx, w := setup(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := restriction.DropGrantsIn(ctx, []uuid.UUID{w.project}, w.ivy, w.maya); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t, grantRows, w.secret, w.ivy); n != 1 {
		t.Fatalf("Ivy's grant: %d rows, want it kept after she left the project", n)
	}
	if at, by := w.mark(t, grantMark, w.secret, w.ivy); !at.Valid || by.UUID != w.maya {
		t.Fatalf("Ivy's grant is marked %v by %v after she left the project, want taken off by Maya", at, by)
	}
	if got := grantsOf(t, ctx, w.secret); !slices.Equal(got, []uuid.UUID{w.hal}) {
		t.Fatalf("grants %v, want Hal's alone", got)
	}
	if letIn(t, ctx, w.ivy, w.secret) {
		t.Fatal("the rule still lets Ivy in after she left the project")
	}
	// Back in the project and let in again, her own row comes back.
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, live := w.count(t, grantRows, w.secret, w.ivy), w.count(t, liveGrantRows, w.secret, w.ivy); n != 1 || live != 1 {
		t.Fatalf("Ivy let in again: %d rows, %d live, want her one row live", n, live)
	}
}

// granted is a grant's who and when.
func (w world) granted(t *testing.T, task, who uuid.UUID) (uuid.NullUUID, time.Time) {
	t.Helper()
	var (
		by uuid.NullUUID
		at time.Time
	)
	if err := w.db.QueryRow(`SELECT granted_by, granted_at FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`, task, who).Scan(&by, &at); err != nil {
		t.Fatal(err)
	}
	return by, at
}

// Maya takes Hal off. Ana saving the list again without him, Ana lifting the
// restriction and Hal leaving the project each leave his mark as Maya made
// it: a later mark never writes over the record of an earlier removal.
func TestV179AnEarlierRemovalKeepsItsWhoAndWhen(t *testing.T) {
	ctx, w := setup(t)
	first := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	off := first.Add(10 * time.Minute)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, nil, off); err != nil {
		t.Fatal(err)
	}
	kept := func(after string) {
		t.Helper()
		if at, by := w.mark(t, grantMark, w.secret, w.hal); !at.Valid || !at.Time.Equal(off) || by.UUID != w.maya {
			t.Fatalf("after %s, Hal's grant is marked %v by %v, want %v by Maya, who took him off", after, at, by, off)
		}
	}
	kept("Maya took him off")
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.ivy}, nil, off.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	kept("Ana saved the list again without him")
	if changed, err := restriction.Unrestrict(ctx, w.secret, w.ana); err != nil || !changed {
		t.Fatalf("lifting it: changed %v, %v", changed, err)
	}
	kept("Ana lifted the restriction")
	// Ivy's grant was live, so the lift marks it, by Ana.
	if at, by := w.mark(t, grantMark, w.secret, w.ivy); !at.Valid || by.UUID != w.ana {
		t.Fatalf("Ivy's grant is marked %v by %v, want by Ana, who lifted the restriction", at, by)
	}
	if err := restriction.DropGrantsIn(ctx, []uuid.UUID{w.project}, w.hal, w.ana); err != nil {
		t.Fatal(err)
	}
	kept("Hal left the project")
}

// Maya opens the task below on its own. Ana lifting the task above later
// leaves the one below marked as Maya opened it.
func TestV179ATaskBelowOpenedOnItsOwnKeepsItsMark(t *testing.T) {
	ctx, w := setup(t)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, []uuid.UUID{w.below}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if changed, err := restriction.Unrestrict(ctx, w.below, w.maya); err != nil || !changed {
		t.Fatalf("opening the task below: changed %v, %v", changed, err)
	}
	opened, by := w.mark(t, restrictionMark, w.below)
	if !opened.Valid || by.UUID != w.maya {
		t.Fatalf("the task below is marked %v by %v, want opened by Maya", opened, by)
	}
	if changed, err := restriction.Unrestrict(ctx, w.secret, w.ana); err != nil || !changed {
		t.Fatalf("lifting the task above: changed %v, %v", changed, err)
	}
	if at, by := w.mark(t, restrictionMark, w.below); !at.Valid || !at.Time.Equal(opened.Time) || by.UUID != w.maya {
		t.Fatalf("after Ana lifted the task above, the task below is marked %v by %v, want %v by Maya", at, by, opened.Time)
	}
	if at, by := w.mark(t, restrictionMark, w.secret); !at.Valid || by.UUID != w.ana {
		t.Fatalf("the task above is marked %v by %v, want lifted by Ana", at, by)
	}
}

// A live restriction restricted again by Ana still says Maya restricted it,
// and when (the task's restriction answer shows both). Once it's lifted, Ana
// restricting it again says Ana, and her time.
func TestV179RestrictingAgainKeepsTheFirstRestrictorUntilLifted(t *testing.T) {
	ctx, w := setup(t)
	first := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.ivy, w.hal}, nil, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, err := restriction.Of(ctx, w.secret); err != nil || !r.Restricted || r.RestrictedBy != w.maya || !r.RestrictedAt.Equal(first) {
		t.Fatalf("restricted again while restricted: %+v (%v), want restricted by Maya at %v", r, err, first)
	}
	if changed, err := restriction.Unrestrict(ctx, w.secret, w.maya); err != nil || !changed {
		t.Fatalf("lifting it: changed %v, %v", changed, err)
	}
	later := first.Add(2 * time.Hour)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.hal}, nil, later); err != nil {
		t.Fatal(err)
	}
	if r, err := restriction.Of(ctx, w.secret); err != nil || !r.Restricted || r.RestrictedBy != w.ana || !r.RestrictedAt.Equal(later) {
		t.Fatalf("restricted afresh after it was lifted: %+v (%v), want restricted by Ana at %v", r, err, later)
	}
}

// A task below that is restricted on its own, with its own people, stays
// restricted on its own when the task above is restricted with it named
// below: the task above's people don't replace its own.
func TestV179ATaskBelowRestrictedOnItsOwnStaysSo(t *testing.T) {
	ctx, w := setup(t)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.below, w.project, w.maya, []uuid.UUID{w.hal}, nil, at); err != nil {
		t.Fatal(err)
	}
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.ivy}, []uuid.UUID{w.below}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, err := restriction.Of(ctx, w.below)
	if err != nil || !r.Restricted || r.Via != uuid.Nil || r.RestrictedBy != w.maya || !r.RestrictedAt.Equal(at) || !slices.Equal(r.Grants, []uuid.UUID{w.hal}) {
		t.Fatalf("the task below: %+v (%v), want restricted on its own by Maya at %v, to Hal", r, err, at)
	}
	if !letIn(t, ctx, w.hal, w.below) || letIn(t, ctx, w.ivy, w.below) {
		t.Fatal("the task below lets in the task above's people instead of its own")
	}
}

// Hal, taken off by Maya and let in again by Ana, holds a grant from Ana,
// from then. Ivy, let in throughout, keeps Maya's.
func TestV179APersonLetInAgainIsGrantedAnew(t *testing.T) {
	ctx, w := setup(t)
	first := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy, w.hal}, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := restriction.Restrict(ctx, w.secret, w.project, w.maya, []uuid.UUID{w.ivy}, nil, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again := first.Add(2 * time.Hour)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.ivy, w.hal}, nil, again); err != nil {
		t.Fatal(err)
	}
	if by, at := w.granted(t, w.secret, w.hal); by.UUID != w.ana || !at.Equal(again) {
		t.Fatalf("Hal let in again holds a grant from %v at %v, want from Ana at %v", by, at, again)
	}
	if by, at := w.granted(t, w.secret, w.ivy); by.UUID != w.maya || !at.Equal(first) {
		t.Fatalf("Ivy, let in throughout, holds a grant from %v at %v, want Maya's at %v", by, at, first)
	}
}
