//go:build integration
// +build integration

package integration_test

// SLA alerts (linear-24 H2) against real Postgres, Redis and Dgraph, in
// task_push_test.go's world with "Spring launch"'s SLAs on: the projects.sla
// sweep tells a task's assignee once when its timer goes at risk and once when
// it breaches, however many sweeps run (each claims its alerts with one
// conditional UPDATE); a paused member's push waits, and a guest with a
// member's place, someone taken off the team and a task given to nobody tell
// nobody; a moved due tells again. Timers are kept rows placed around now.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestSLAAlertsTellTheAssigneeOnceAcrossSweeps|TestSLAAlertClaimsTakeEachOnce|TestSLAAlertClaimsAtOnceTakeEachOnce' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	_ "github.com/akashc777/OneCamp/business/SLA"
	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	slaModel "github.com/akashc777/OneCamp/models/postgres/SLA"
)

// slaWorld is task_push_test.go's world with Spring launch's SLAs on and Gus,
// a guest with a member's place in it, hearing of everything.
func slaWorld(t *testing.T) *taskWorld {
	t.Helper()
	w := newTaskWorld(t)
	ctx := context.Background()
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsSLA.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.launch},
		json.RawMessage("true"), policyBusiness.Changer{UserID: w.people["maya"], Email: "maya@acme.test"}); err != nil {
		t.Fatal(err)
	}
	gus := uuid.New()
	w.people["gus"] = gus
	for _, q := range []string{
		`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at, is_external, is_bot) VALUES ($1, 'gus@guest.test', 'gus', 'gus', NOW(), NOW(), true, false)`,
		`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-gus')`,
		`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-gus')`,
		`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, 'all')`,
	} {
		args := []any{gus}
		if strings.Count(q, "$") == 2 {
			args = append(args, w.launch)
		}
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	uids := w.dg.Mutate(t, []map[string]any{
		{"uid": "_:gus", "dgraph.type": "User", "user_uuid": gus.String(), "user_name": "gus", "user_deleted_at": "0001-01-01T00:00:00Z", "is_external": true},
		{"uid": w.uids["launch"], "project_members": []map[string]any{{"uid": "_:gus"}}},
	})
	w.uids["gus"] = uids["gus"]
	return w
}

// timed keeps a running timer for task, at risk from risk and due at due.
func (w *taskWorld) timed(t *testing.T, task string, risk, due time.Time) {
	t.Helper()
	if _, err := w.env.PG.Exec(`INSERT INTO task_slas (task_uuid, project_uuid, source, started_at, due_at, risk_at, state)
		VALUES ($1, $2, 'app', NOW() - interval '2 days', $3, $4, 'running')`, task, w.launch, due, risk); err != nil {
		t.Fatal(err)
	}
}

// slaTold is, for one person, their shown Inbox items of kind, their SLA
// emails (and of those, how many wait for a hold), and their pushes whose
// title says title.
func (w *taskWorld) slaTold(t *testing.T, name, kind, title string) [4]int {
	t.Helper()
	var got [4]int
	id := w.people[name]
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND reason <> 'listed'`, id, kind).Scan(&got[0])
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.sla'`, id).Scan(&got[1])
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.sla' AND next_retry_at > NOW()`, id).Scan(&got[2])
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK && strings.Contains(p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE], title) {
			for _, tok := range p.tokens {
				if tok == "tok-"+name {
					got[3]++
				}
			}
		}
	}
	return got
}

func (w *taskWorld) expectSLA(t *testing.T, what, name, kind, title string, want [4]int) {
	t.Helper()
	var got [4]int
	if !within(10*time.Second, func() bool { got = w.slaTold(t, name, kind, title); return got == want }) {
		if got = w.slaTold(t, name, kind, title); got != want {
			t.Errorf("%s: %s told [inbox emails held pushes] %v, want %v", what, name, got, want)
		}
	}
}

func (w *taskWorld) sweep(t *testing.T) {
	t.Helper()
	if err := workSweep.SLA(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSLAAlertsTellTheAssigneeOnceAcrossSweeps(t *testing.T) {
	w := slaWorld(t)
	now := time.Now()
	risk, due, past := now.Add(-time.Minute), now.Add(time.Hour), now.Add(-time.Minute)

	video := w.urgentTask(t, "Launch video", "ben")
	w.timed(t, video, risk, due)
	// Tom hears of mentions only: an alert of his own task is for him.
	late := w.urgentTask(t, "Press list", "tom")
	w.timed(t, late, past.Add(-time.Hour), past)
	// Kay paused her notifications: no push; the email waits for the pause.
	if _, err := w.env.PG.Exec(`UPDATE users_notification_preferences SET notifications_paused_until = NOW() + interval '2 hours' WHERE user_id = $1`, w.people["kay"]); err != nil {
		t.Fatal(err)
	}
	w.timed(t, w.urgentTask(t, "Kay's", "kay"), risk, due)
	// Lee was taken off the team; Gus is a guest; nobody has the last.
	w.timed(t, w.urgentTask(t, "Lee's", "lee"), risk, due)
	w.timed(t, w.urgentTask(t, "Gus's", "gus"), risk, due)
	nobody := w.urgentTask(t, "Nobody's", "")
	w.timed(t, nobody, risk, due)
	// Maya restricted the vault, and Sam is its assignee: he may read it, so
	// the sweep, which reads every task's alert as the system, tells him.
	vault := w.urgentTask(t, "Vault", "sam")
	if _, err := w.env.PG.Exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, vault, w.launch, w.people["maya"]); err != nil {
		t.Fatal(err)
	}
	w.timed(t, vault, risk, due)

	w.sweep(t)
	w.sweep(t)
	w.expectSLA(t, "at risk, two sweeps", "ben", "task.sla.at_risk", "Launch video is at risk", [4]int{1, 1, 0, 1})
	w.expectSLA(t, "past due, two sweeps", "tom", "task.sla.breached", "Press list breached its SLA", [4]int{1, 1, 0, 1})
	w.expectSLA(t, "paused", "kay", "task.sla.at_risk", "Kay's is at risk", [4]int{1, 1, 1, 0})
	w.expectSLA(t, "taken off the team", "lee", "task.sla.at_risk", "SLA", [4]int{0, 0, 0, 0})
	w.expectSLA(t, "a guest", "gus", "task.sla.at_risk", "SLA", [4]int{0, 0, 0, 0})
	w.expectSLA(t, "a restricted task, its assignee", "sam", "task.sla.at_risk", "Vault is at risk", [4]int{1, 1, 0, 1})
	var any int
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE resource_uuid = $1`, nobody).Scan(&any)
	if any != 0 {
		t.Errorf("a task given to nobody told %d", any)
	}
	if got := w.slaTold(t, "tom", "task.sla.at_risk", "at risk"); got[0] != 0 {
		t.Errorf("found past due, Tom was also told it was at risk: %v", got)
	}

	// Its due moves an hour on (a new rule): it is told again, once.
	moved := due.Add(time.Hour)
	tm, found, err := slaModel.TimerOf(context.Background(), uuid.MustParse(video))
	if err != nil || !found {
		t.Fatalf("the video's timer: %v %v", found, err)
	}
	tm.DueAt, tm.RiskAt = &moved, &risk
	if err := slaModel.PutTimer(context.Background(), tm); err != nil {
		t.Fatal(err)
	}
	w.sweep(t)
	w.sweep(t)
	w.expectSLA(t, "a moved due", "ben", "task.sla.at_risk", "Launch video is at risk", [4]int{2, 2, 0, 2})
}

// Two sweeps (two servers) that read the same due alerts before either
// claims them: each alert is claimed once.
func TestSLAAlertClaimsTakeEachOnce(t *testing.T) {
	w := slaWorld(t)
	ctx := context.Background()
	// To the microsecond: the app's pgx truncates a time to what Postgres
	// keeps, while this test's lib/pq sends nanoseconds Postgres rounds, so
	// "warned_at = now" below would miss half the time.
	now := time.Now().Truncate(time.Microsecond)
	a, b := w.urgentTask(t, "A", "ben"), w.urgentTask(t, "B", "sam")
	w.timed(t, a, now.Add(-time.Minute), now.Add(time.Hour))
	w.timed(t, b, now.Add(-2*time.Hour), now.Add(-time.Minute))
	first, err1 := slaModel.Due(ctx, now, 10)
	second, err2 := slaModel.Due(ctx, now, 10)
	if err1 != nil || err2 != nil || len(first) != 2 || len(second) != 2 {
		t.Fatalf("due: %v %v, %v %v", first, err1, second, err2)
	}
	ids := []uuid.UUID{first[0].TaskID, first[1].TaskID}
	took, err := slaModel.Claim(ctx, ids, now)
	if err != nil || len(took) != 2 {
		t.Fatalf("the first claim took %v (%v), want both", took, err)
	}
	again, err := slaModel.Claim(ctx, ids, now.Add(time.Second))
	if err != nil || len(again) != 0 {
		t.Errorf("the second claim took %v (%v): told twice", again, err)
	}
	if left, err := slaModel.Due(ctx, now.Add(time.Second), 10); err != nil || len(left) != 0 {
		t.Errorf("still due after the claim: %v %v", left, err)
	}
	var breached, warned int
	_ = w.env.PG.QueryRow(`SELECT count(*) FILTER (WHERE breach_told_at IS NOT NULL), count(*) FILTER (WHERE warned_at = $2) FROM task_slas WHERE task_uuid = ANY($1::uuid[])`,
		"{"+a+","+b+"}", now).Scan(&breached, &warned)
	if breached != 1 || warned != 2 {
		t.Errorf("breach told %d (want B's), warned at the first claim %d (want 2)", breached, warned)
	}
}

// Eight sweeps (eight servers) claiming the same 30 due alerts at the same
// instant, five rounds over: each alert is taken exactly once, and as its own
// kind (at risk or breached). Claims one after another can't tell an atomic
// claim from a check-then-act one; this can. The app's pool is warmed first,
// so the eight claims hold their own connections and truly overlap.
func TestSLAAlertClaimsAtOnceTakeEachOnce(t *testing.T) {
	w := slaWorld(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	const alerts, sweeps, rounds = 30, 8, 5
	ids := make([]uuid.UUID, 0, alerts)
	breach := map[uuid.UUID]bool{}
	for i := 0; i < alerts; i++ {
		id := uuid.MustParse(w.urgentTask(t, fmt.Sprintf("Alert %d", i), "ben"))
		due := now.Add(time.Hour) // at risk
		if i%2 == 1 {
			due = now.Add(-time.Minute) // breached
		}
		w.timed(t, id.String(), now.Add(-2*time.Hour), due)
		ids = append(ids, id)
		breach[id] = i%2 == 1
	}
	var warm sync.WaitGroup
	for s := 0; s < sweeps; s++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			if _, err := pgexec.Exec(ctx, `SELECT pg_sleep(0.3)`); err != nil {
				t.Error(err)
			}
		}()
	}
	warm.Wait()
	for round := 0; round < rounds; round++ {
		if _, err := w.env.PG.Exec(`UPDATE task_slas SET warned_at = NULL, breach_told_at = NULL WHERE task_uuid = ANY($1::uuid[])`, pq.Array(ids)); err != nil {
			t.Fatal(err)
		}
		var (
			mu    sync.Mutex
			took  = map[uuid.UUID]int{}
			kinds = map[uuid.UUID]bool{}
			wg    sync.WaitGroup
			start = make(chan struct{})
		)
		for s := 0; s < sweeps; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := slaModel.Claim(ctx, ids, now)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, a := range got {
					took[a.TaskID]++
					kinds[a.TaskID] = a.Breach
				}
			}()
		}
		close(start)
		wg.Wait()
		var notOnce, wrongKind int
		for _, id := range ids {
			if took[id] != 1 {
				notOnce++
			} else if kinds[id] != breach[id] {
				wrongKind++
			}
		}
		if notOnce != 0 || wrongKind != 0 {
			t.Errorf("round %d: %d of %d alerts not taken exactly once by %d sweeps at once, %d taken as the wrong kind", round, notOnce, alerts, sweeps, wrongKind)
		}
	}
}
