//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/models/pgexec"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	"github.com/akashc777/OneCamp/models/taskread"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// pgStatements is a pgx tracer that keeps the text of every statement.
type pgStatements struct {
	mu   sync.Mutex
	sqls []string
}

func (p *pgStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	p.mu.Lock()
	p.sqls = append(p.sqls, d.SQL)
	p.mu.Unlock()
	return ctx
}

func (p *pgStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *pgStatements) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.sqls
	p.sqls = nil
	return out
}

// countPostgres makes every pgexec statement go through a connection that
// counts it, for the rest of the test.
func countPostgres(t *testing.T, dsn string) *pgStatements {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	counter := &pgStatements{}
	cfg.Tracer = counter
	db := stdlib.OpenDB(*cfg)
	was := postgresInit.DBConn.SqlDB
	postgresInit.DBConn.SqlDB = db
	t.Cleanup(func() {
		postgresInit.DBConn.SqlDB = was
		_ = db.Close()
	})
	return counter
}

// decisionReads is how many of the statements are the task-read decision's
// own reads: what waits in triage, and the restrictions.
func decisionReads(sqls []string) (n int) {
	for _, q := range sqls {
		if strings.Contains(q, "triage_items") || strings.Contains(q, "task_restrictions") {
			n++
		}
	}
	return n
}

type decisionWorld struct {
	*workWorld
	pg   *pgStatements
	call func(who string) (taskread.Filter, int)
}

// A seeded project, reached through VerifyAuth: each call is one request, as
// the person named, that takes the task-read decision.
func newDecisionWorld(t *testing.T) *decisionWorld {
	t.Helper()
	t.Setenv("JWT_SECRET", "taskread-generation-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	// Entries never age out here: a re-read is the generation's doing, never
	// MaxAge's, however long a loaded machine takes between requests.
	t.Cleanup(taskReadGeneration.HoldClockForTest())
	d := &decisionWorld{workWorld: w, pg: countPostgres(t, w.env.DSN)}
	var got taskread.Filter
	// Through the first middleware, as the router has it: it gives the request
	// its place for the decision and says whether a kept one may serve it.
	h := middleware.RequestIDMiddleware(middleware.VerifyAuth(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f, err := taskread.From(r.Context())
		if err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		got = f
		rw.WriteHeader(http.StatusOK)
	})))
	d.call = func(who string) (taskread.Filter, int) {
		t.Helper()
		session, err := helpers.SignSessionToken(w.people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/project/tasks", nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: session})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		d.pg.take()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request as %s: %d", who, rec.Code)
		}
		return got, decisionReads(d.pg.take())
	}
	return d
}

func (d *decisionWorld) triageOn(t *testing.T) {
	t.Helper()
	by := policyBusiness.Changer{UserID: d.people["ana"], Email: "ana@acme.test"}
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: d.project}
	if _, err := policyBusiness.Admin.SetIn(context.Background(), policyBusiness.ProjectsTriage.Key, scope, json.RawMessage("true"), by); err != nil {
		t.Fatal(err)
	}
}

func leavesOut(f taskread.Filter, task string) bool {
	return f.Hides(task) || strings.Contains(f.Cond(), strings.ToLower(task))
}

// A1-02: the decision's Postgres reads are kept between requests while
// nothing they read has changed: the second request reads neither table.
func TestTaskReadDecisionIsReadOncePerGeneration(t *testing.T) {
	d := newDecisionWorld(t)
	_, first := d.call("ben")
	_, second := d.call("ben")
	if first == 0 {
		t.Fatalf("the first request read nothing for the decision")
	}
	if second != 0 {
		t.Fatalf("a second request with nothing changed made %d decision reads, want 0", second)
	}
}

// A1-02: a task put in triage between two requests is left out of the
// second, which reads again.
func TestATriageWriteBetweenRequestsIsSeen(t *testing.T) {
	d := newDecisionWorld(t)
	d.triageOn(t)
	if f, _ := d.call("ben"); leavesOut(f, d.task) {
		t.Fatal("the task waits before it was put in triage")
	}
	d.call("ben")
	project, task := uuid.MustParse(d.project), uuid.MustParse(d.task)
	if err := triageModel.PutAll(context.Background(), []triageModel.Item{{TaskUUID: task, ProjectUUID: project, Source: "form", RequesterName: "Visitor"}}); err != nil {
		t.Fatal(err)
	}
	f, n := d.call("ben")
	if n == 0 {
		t.Fatal("a triage write did not make the next request read again")
	}
	if !leavesOut(f, d.task) {
		t.Fatal("the task put in triage is still on ben's lists")
	}
}

// A1-02: a task restricted between two requests is hidden on the second.
func TestARestrictionWriteBetweenRequestsIsSeen(t *testing.T) {
	d := newDecisionWorld(t)
	ana := d.account("ana")
	if f, _ := d.call("ben"); f.Hides(d.task) {
		t.Fatal("the task is hidden before it was restricted")
	}
	d.call("ben")
	ctx := context.Background()
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, d.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	f, n := d.call("ben")
	if n == 0 {
		t.Fatal("a restriction write did not make the next request read again")
	}
	if !f.Hides(d.task) {
		t.Fatal("the task restricted to its maker is still on ben's lists")
	}
}

// A1-02: another process's write reaches this one through the generation in
// Redis: a write here without a bump, then the bump alone, makes the next
// request read again.
func TestAnotherProcessesBumpMakesTheNextRequestReadAgain(t *testing.T) {
	d := newDecisionWorld(t)
	d.call("ben")
	d.call("ben")
	d.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, d.task, d.project, d.people["ana"])
	if err := redisInit.RedisClient.Incr(context.Background(), "taskread:gen").Err(); err != nil {
		t.Fatal(err)
	}
	f, n := d.call("ben")
	if n == 0 {
		t.Fatal("a bumped generation did not make the next request read again")
	}
	if !f.Hides(d.task) {
		t.Fatal("the task another process restricted is still on ben's lists")
	}
}

// A1-02: a write moves the generation every process reads, after its
// commit, and every later write moves it again; a write rolled back moves
// nothing.
func TestACommittedWriteMovesTheSharedGeneration(t *testing.T) {
	d := newDecisionWorld(t)
	generation := func() string {
		v, _ := redisInit.RedisClient.Get(context.Background(), "taskread:gen").Result()
		return v
	}
	project, task := uuid.MustParse(d.project), uuid.MustParse(d.task)
	put := func(ctx context.Context) error {
		return triageModel.PutAll(ctx, []triageModel.Item{{TaskUUID: task, ProjectUUID: project, Source: "form", RequesterName: "Visitor"}})
	}
	rolledBack := errors.New("rolled back")
	if err := pgexec.InTx(context.Background(), func(ctx context.Context) error {
		if err := put(ctx); err != nil {
			return err
		}
		return rolledBack
	}); !errors.Is(err, rolledBack) {
		t.Fatalf("the write meant to roll back: %v", err)
	}
	if g := generation(); g != "" {
		t.Fatalf("a write rolled back moved the generation to %s", g)
	}
	if err := put(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := generation()
	if first == "" {
		t.Fatal("a committed write left the generation where it was")
	}
	if err := triageModel.EndWait(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if generation() == first {
		t.Fatal("a second committed write left the generation where the first put it")
	}
}

// CR-376 F1: every write to the decision's rows makes the next request read
// them again, in every process. Each writer runs as another process's
// (taskReadGeneration.AsAnotherProcessForTest puts this process's own count
// back), so only the shared generation in Redis can make the next request
// read again: a writer whose bump is lost fails here. Where a lost bump would
// show a task to someone it hides from (InheritAll, DropGrantsIn) the row
// checks the decision too.
func TestEveryWriteToTheDecisionsRowsMovesItInEveryProcess(t *testing.T) {
	ctx := context.Background()
	type world struct {
		*decisionWorld
		task, project, ana, ben, sub uuid.UUID
	}
	put := func(w world) {
		if err := triageModel.PutAll(ctx, []triageModel.Item{{TaskUUID: w.task, ProjectUUID: w.project, Source: "form", RequesterName: "Visitor"}}); err != nil {
			w.t.Fatal(err)
		}
	}
	restrict := func(w world, grants ...uuid.UUID) {
		if err := restrictionModel.Restrict(ctx, w.task, w.project, w.ana, grants, nil, time.Now()); err != nil {
			w.t.Fatal(err)
		}
	}
	for _, row := range []struct {
		writer string
		before func(w world)
		write  func(w world) error
		// after checks the decision the write changed for ben.
		after func(w world, f taskread.Filter) string
	}{
		{writer: "Triage.PutAll", before: func(w world) { w.triageOn(w.t) },
			write: func(w world) error { put(w); return nil },
			after: func(w world, f taskread.Filter) string {
				if !leavesOut(f, w.task.String()) {
					return "the task put in triage is still on ben's lists"
				}
				return ""
			}},
		{writer: "Triage.EndWait", before: func(w world) { w.triageOn(w.t); put(w) },
			write: func(w world) error { return triageModel.EndWait(ctx, w.task) },
			after: func(w world, f taskread.Filter) string {
				if leavesOut(f, w.task.String()) {
					return "the task whose wait ended is still left out of ben's lists"
				}
				return ""
			}},
		{writer: "Triage.Decide", before: func(w world) { w.triageOn(w.t); put(w) },
			write: func(w world) error {
				found, _, err := triageModel.Decide(ctx, triageModel.Decision{TaskUUID: w.task, ProjectUUID: w.project, Decision: "accept", DecidedBy: w.ana},
					func(context.Context) error { return nil })
				if err == nil && !found {
					err = errors.New("the task was not waiting")
				}
				return err
			},
			after: func(w world, f taskread.Filter) string {
				if leavesOut(f, w.task.String()) {
					return "the task accepted out of triage is still left out of ben's lists"
				}
				return ""
			}},
		{writer: "TaskRestriction.Restrict",
			write: func(w world) error { restrict(w); return nil },
			after: func(w world, f taskread.Filter) string {
				if !f.Hides(w.task.String()) {
					return "the task restricted to its maker is still on ben's lists"
				}
				return ""
			}},
		{writer: "TaskRestriction.InheritAll", before: func(w world) { restrict(w) },
			write: func(w world) error {
				return restrictionModel.InheritAll(ctx, []uuid.UUID{w.sub}, []uuid.UUID{w.task}, w.ana, time.Now())
			},
			after: func(w world, f taskread.Filter) string {
				if !f.Hides(w.sub.String()) {
					return "a sub-task made under a restricted task is on the lists of ben, who isn't let into it"
				}
				return ""
			}},
		{writer: "TaskRestriction.Unrestrict", before: func(w world) { restrict(w) },
			write: func(w world) error {
				lifted, err := restrictionModel.Unrestrict(ctx, w.task, w.ana)
				if err == nil && !lifted {
					err = errors.New("the task was not restricted")
				}
				return err
			},
			after: func(w world, f taskread.Filter) string {
				if f.Hides(w.task.String()) {
					return "the task opened to its project is still hidden from ben"
				}
				return ""
			}},
		{writer: "TaskRestriction.DropGrantsIn", before: func(w world) { restrict(w, w.ben) },
			write: func(w world) error { return restrictionModel.DropGrantsIn(ctx, []uuid.UUID{w.project}, w.ben, w.ana) },
			after: func(w world, f taskread.Filter) string {
				if !f.Hides(w.task.String()) {
					return "ben's grant on the restricted task was dropped, and it still lets him in"
				}
				return ""
			}},
	} {
		t.Run(row.writer, func(t *testing.T) {
			d := newDecisionWorld(t)
			w := world{decisionWorld: d, task: uuid.MustParse(d.task), project: uuid.MustParse(d.project),
				ana: d.people["ana"], ben: d.people["ben"], sub: uuid.New()}
			d.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, w.sub, w.project, w.ana)
			if row.before != nil {
				row.before(w)
			}
			if _, n := d.call("ben"); n == 0 {
				t.Fatal("the first request read nothing for the decision")
			}
			if f, n := d.call("ben"); n != 0 {
				t.Fatalf("a second request with nothing written made %d decision reads, want 0", n)
			} else if row.writer == "TaskRestriction.DropGrantsIn" && f.Hides(w.task.String()) {
				t.Fatal("ben's grant does not let him into the restricted task")
			} else if row.writer == "TaskRestriction.InheritAll" && f.Hides(w.sub.String()) {
				t.Fatal("the sub-task is hidden before it inherited the restriction")
			}
			generation := func() int64 {
				n, err := redisInit.RedisClient.Get(ctx, "taskread:gen").Int64()
				if err != nil && !errors.Is(err, redis.Nil) {
					t.Fatal(err)
				}
				return n
			}
			was := generation()
			var err error
			taskReadGeneration.AsAnotherProcessForTest(func() { err = row.write(w) })
			if err != nil {
				t.Fatalf("%s: %v", row.writer, err)
			}
			if now := generation(); now <= was {
				t.Fatalf("%s left the shared generation at %d (was %d)", row.writer, now, was)
			}
			f, n := d.call("ben")
			if n == 0 {
				t.Fatalf("after %s the next request did not read the decision again", row.writer)
			}
			if row.after != nil {
				if bad := row.after(w, f); bad != "" {
					t.Fatal(bad)
				}
			}
		})
	}
}
