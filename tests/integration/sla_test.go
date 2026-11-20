//go:build integration
// +build integration

package integration_test

// SLA timers (linear-24 H1, migration 596) through the app's own routes and
// its Work Change Log sink: a project's SLA settings are read by its members
// and changed by its admins, each change audited (a member is refused, someone
// outside finds nothing); a task's timer counts the project's business clock
// in its zone, keeps one row and its start however often a change is
// delivered, ends kept, settles met or missed by when the task was done, and
// rides only its members' reads (never a guest's); a removed rule is kept and
// times nothing new; and 596 goes up, down and up again.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	formula "github.com/akashc777/OneCamp/business/Formula"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	slaBusiness "github.com/akashc777/OneCamp/business/SLA"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	slaController "github.com/akashc777/OneCamp/controllers/SLA"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	slaModel "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/akashc777/OneCamp/tests/integration"
)

// slaCall runs a handler as who, through the request middleware, with the
// route parameters given and body (nil for none) as its JSON.
func slaCall(w *workWorld, who, method string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
	w.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(method, "/", rdr).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

type slaRuleRead struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Priorities    []string `json:"priorities"`
	TargetMinutes int      `json:"target_minutes"`
	Virtual       bool     `json:"virtual"`
}

type slaSettingsRead struct {
	On        bool          `json:"on"`
	Rules     []slaRuleRead `json:"rules"`
	Defaults  bool          `json:"defaults"`
	HoursSet  bool          `json:"hours_set"`
	CanManage bool          `json:"can_manage"`
}

// slaShow is GET /project/{project_uuid}/sla as who.
func slaShow(w *workWorld, who string) (int, slaSettingsRead, string) {
	w.t.Helper()
	code, out := slaCall(w, who, http.MethodGet, slaController.Show, map[string]string{"project_uuid": w.project}, nil)
	var got struct {
		Data slaSettingsRead `json:"data"`
	}
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			w.t.Fatalf("the SLA settings as %s: %v in %s", who, err, out)
		}
	}
	return code, got.Data, out
}

// slaAudits is how many admin audit rows name action (a LIKE pattern) for
// the world's project, once the queued rows are written.
func slaAudits(w *workWorld, action string) int {
	w.t.Helper()
	integration.Settle(w.t)
	var n int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action LIKE $1 AND metadata->>'project_uuid' = $2`,
		action, w.project).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func slaListed(rules []slaRuleRead, id string) bool {
	for _, r := range rules {
		if r.ID == id {
			return true
		}
	}
	return false
}

// slaGuest is Gus, a guest (an external identity, not a bot) with a
// member's place in the world's project: a role row and the graph's link.
func slaGuest(w *workWorld) uuid.UUID {
	w.t.Helper()
	gus := uuid.New()
	w.people["gus"] = gus
	w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at, is_external, is_bot)
		VALUES ($1, 'gus@guest.test', 'gus', 'gus', NOW(), NOW(), true, false)`, gus)
	var proj struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
	}
	if raw := w.dg.Query(w.t, `{ p(func: eq(project_uuid, "`+w.project+`")) { uid } }`); json.Unmarshal(raw, &proj) != nil || len(proj.P) != 1 {
		w.t.Fatalf("the project in the graph: %s", raw)
	}
	w.dg.Mutate(w.t, []map[string]any{
		{"uid": "_:gus", "dgraph.type": "User", "user_uuid": gus.String(), "user_name": "gus", "user_deleted_at": "0001-01-01T00:00:00Z",
			"is_external": true, "user_projects": []map[string]any{{"uid": proj.P[0].UID}}},
		{"uid": proj.P[0].UID, "project_members": []map[string]any{{"uid": "_:gus"}}},
	})
	w.exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'member')`, w.project, gus)
	return gus
}

func TestProjectSLAsAreReadByMembersAndSetByAdmins(t *testing.T) {
	w := seedWorkWorld(t)
	p := map[string]string{"project_uuid": w.project}
	ruleOf := func(id string) map[string]string { return map[string]string{"project_uuid": w.project, "rule_id": id} }

	// Ana is the project's admin and Ben a member: both read the defaults,
	// Urgent and High; only Ana may change them. Eve, outside it, finds nothing.
	code, ana, out := slaShow(w, "ana")
	if code != http.StatusOK || !ana.Defaults || len(ana.Rules) != 2 || ana.On || !ana.CanManage {
		t.Fatalf("the admin's first read: %d %s, want the two defaults, off, can_manage", code, out)
	}
	if code, ben, out := slaShow(w, "ben"); code != http.StatusOK || !ben.Defaults || len(ben.Rules) != 2 || ben.CanManage {
		t.Fatalf("a member's read: %d %s, want the two defaults and no can_manage", code, out)
	}
	if code, _, out := slaShow(w, "eve"); code != http.StatusNotFound {
		t.Fatalf("someone outside the project reads its SLAs: %d %s, want 404", code, out)
	}
	// Gus, a guest with a member's place, reads none of them: they are SLA
	// state, as a task's timer is.
	slaGuest(w)
	if code, _, out := slaShow(w, "gus"); code != http.StatusForbidden {
		t.Fatalf("a guest reads the project's SLAs: %d %s, want 403", code, out)
	}
	urgent := ana.Rules[0].ID

	hours := map[string]any{"hours": map[string]any{"days": []int{1, 2, 3, 4, 5}, "start": "09:00", "end": "17:00", "tz": "Asia/Kolkata"}}
	rule := map[string]any{"name": "Bugs", "priorities": []string{"medium"}, "labels": []string{"bug"}, "target_minutes": 240, "clock": "calendar"}
	changes := []struct {
		what, method string
		h            http.HandlerFunc
		params       map[string]string
		body         any
	}{
		{"sets the hours", http.MethodPut, slaController.SetHours, p, hours},
		{"adds a rule", http.MethodPost, slaController.AddRule, p, rule},
		{"changes a rule", http.MethodPatch, slaController.ChangeRule, ruleOf(urgent), rule},
		{"removes a rule", http.MethodDelete, slaController.RemoveRule, ruleOf(urgent), nil},
	}
	// A member is refused each change, and nothing is recorded or made;
	// someone outside finds nothing to change.
	for _, c := range changes {
		if code, out := slaCall(w, "ben", c.method, c.h, c.params, c.body); code != http.StatusForbidden {
			t.Errorf("a member %s: %d %s, want 403", c.what, code, out)
		}
		if code, out := slaCall(w, "eve", c.method, c.h, c.params, c.body); code != http.StatusNotFound {
			t.Errorf("someone outside %s: %d %s, want 404", c.what, code, out)
		}
	}
	if n := slaAudits(w, "project.sla.%"); n != 0 {
		t.Fatalf("refused changes left %d audit rows, want none", n)
	}
	var rows int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM sla_rules WHERE project_uuid = $1`, w.project).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused changes wrote %d rules (%v), want none", rows, err)
	}
	if code, ben, out := slaShow(w, "ben"); code != http.StatusOK || !ben.Defaults || ben.HoursSet {
		t.Fatalf("after the refusals: %d %s, want the defaults and no hours set", code, out)
	}

	// Ana adds a rule: 201, audited, and the defaults are written as the
	// project's own beside it.
	code, out = slaCall(w, "ana", http.MethodPost, slaController.AddRule, p, rule)
	var added struct {
		Data slaRuleRead `json:"data"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(out), &added) != nil || added.Data.ID == "" || added.Data.TargetMinutes != 240 {
		t.Fatalf("the admin adds a rule: %d %s, want 201 and the rule", code, out)
	}
	id := added.Data.ID
	if n := slaAudits(w, "project.sla.rule.added"); n != 1 {
		t.Fatalf("an added rule left %d project.sla.rule.added rows, want 1", n)
	}
	var named string
	if err := w.env.PG.QueryRow(`SELECT metadata->>'rule_id' FROM admin_audit_log WHERE action = 'project.sla.rule.added' AND metadata->>'project_uuid' = $1`,
		w.project).Scan(&named); err != nil || named != id {
		t.Fatalf("the added rule's audit row names %q (%v), want %s", named, err, id)
	}
	if code, read, out := slaShow(w, "ben"); code != http.StatusOK || read.Defaults || len(read.Rules) != 3 || !slaListed(read.Rules, id) {
		t.Fatalf("after the admin's rule: %d %s, want the project's own three", code, out)
	}

	// Changes it: 200, audited.
	rule["target_minutes"] = 480
	code, out = slaCall(w, "ana", http.MethodPatch, slaController.ChangeRule, ruleOf(id), rule)
	var changed struct {
		Data slaRuleRead `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &changed) != nil || changed.Data.ID != id || changed.Data.TargetMinutes != 480 {
		t.Fatalf("the admin changes the rule: %d %s, want 200 and 480 minutes", code, out)
	}
	if n := slaAudits(w, "project.sla.rule.changed"); n != 1 {
		t.Fatalf("a changed rule left %d project.sla.rule.changed rows, want 1", n)
	}

	// Sets the work week: 200, audited, read back as set.
	if code, out := slaCall(w, "ana", http.MethodPut, slaController.SetHours, p, hours); code != http.StatusOK {
		t.Fatalf("the admin sets the hours: %d %s", code, out)
	}
	if n := slaAudits(w, "project.sla.hours.changed"); n != 1 {
		t.Fatalf("set hours left %d project.sla.hours.changed rows, want 1", n)
	}
	if code, read, out := slaShow(w, "ben"); code != http.StatusOK || !read.HoursSet {
		t.Fatalf("after the hours: %d %s, want hours_set", code, out)
	}

	// Removes it: 200, audited, kept as removed by Ana, and no longer listed.
	if code, out := slaCall(w, "ana", http.MethodDelete, slaController.RemoveRule, ruleOf(id), nil); code != http.StatusOK {
		t.Fatalf("the admin removes the rule: %d %s", code, out)
	}
	if n := slaAudits(w, "project.sla.rule.removed"); n != 1 {
		t.Fatalf("a removed rule left %d project.sla.rule.removed rows, want 1", n)
	}
	var deletedAt sql.NullTime
	var deletedBy sql.NullString
	if err := w.env.PG.QueryRow(`SELECT deleted_at, deleted_by::text FROM sla_rules WHERE id = $1`, id).Scan(&deletedAt, &deletedBy); err != nil ||
		!deletedAt.Valid || deletedBy.String != w.people["ana"].String() {
		t.Fatalf("the removed rule's row: deleted_at %v, deleted_by %v (%v), want both set, by ana", deletedAt, deletedBy, err)
	}
	if code, read, out := slaShow(w, "ana"); code != http.StatusOK || slaListed(read.Rules, id) || len(read.Rules) != 2 {
		t.Fatalf("after the removal: %d %s, want it gone from the list", code, out)
	}

	// Restored from the trash: someone outside finds nothing, a member is
	// refused, the admin brings it back, audited, and it is listed again.
	restore := func(who string) (int, string) {
		return slaCall(w, who, http.MethodPost, restoreController.Restore, map[string]string{"kind": "sla_rule", "id": id}, nil)
	}
	if code, out := restore("eve"); code != http.StatusNotFound {
		t.Errorf("someone outside restores the rule: %d %s, want 404", code, out)
	}
	if code, out := restore("ben"); code != http.StatusForbidden {
		t.Errorf("a member restores the rule: %d %s, want 403", code, out)
	}
	if n := slaAudits(w, "project.sla.rule.restored"); n != 0 {
		t.Fatalf("refused restores left %d audit rows, want none", n)
	}
	if code, out := restore("ana"); code != http.StatusOK {
		t.Fatalf("the admin restores the rule: %d %s", code, out)
	}
	if n := slaAudits(w, "project.sla.rule.restored"); n != 1 {
		t.Fatalf("a restored rule left %d project.sla.rule.restored rows, want 1", n)
	}
	if err := w.env.PG.QueryRow(`SELECT deleted_at FROM sla_rules WHERE id = $1`, id).Scan(&deletedAt); err != nil || deletedAt.Valid {
		t.Fatalf("the restored rule's deleted_at: %v (%v), want NULL", deletedAt, err)
	}
	if code, read, out := slaShow(w, "ben"); code != http.StatusOK || !slaListed(read.Rules, id) || len(read.Rules) != 3 {
		t.Fatalf("after the restore: %d %s, want it listed again", code, out)
	}

	// A project keeps at most 50 rules. Removed again, then the project
	// filled to 50 live rules: the next add and the rule's restore answer
	// 400, and nothing is made or recorded.
	if code, out := slaCall(w, "ana", http.MethodDelete, slaController.RemoveRule, ruleOf(id), nil); code != http.StatusOK {
		t.Fatalf("the admin removes the rule again: %d %s", code, out)
	}
	w.exec(`INSERT INTO sla_rules (id, project_uuid, name, target_minutes, rank)
		SELECT md5(random()::text || n)::uuid, $1, 'Filler ' || n, 60, 10 + n FROM generate_series(1, 48) n`, w.project)
	live := func() int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM sla_rules WHERE project_uuid = $1 AND deleted_at IS NULL`, w.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := live(); n != slaBusiness.MaxRules {
		t.Fatalf("filled to %d live rules, want %d", n, slaBusiness.MaxRules)
	}
	recorded := slaAudits(w, "project.sla.rule.%")
	if code, out := slaCall(w, "ana", http.MethodPost, slaController.AddRule, p, rule); code != http.StatusBadRequest {
		t.Errorf("the 51st add: %d %s, want 400", code, out)
	}
	if code, out := restore("ana"); code != http.StatusBadRequest {
		t.Errorf("a restore into a full project: %d %s, want 400", code, out)
	}
	if n, after := live(), slaAudits(w, "project.sla.rule.%"); n != slaBusiness.MaxRules || after != recorded {
		t.Fatalf("refused for a full project: %d live rules, %d audit rows (was %d), want %d and none new", n, after, recorded, slaBusiness.MaxRules)
	}
	// The restore's own bound, counted under the lock the adds take (for a
	// restore that found room before the project filled), refuses it too.
	full := errors.New("full")
	back, err := slaModel.RestoreRule(context.Background(), uuid.MustParse(w.project), uuid.MustParse(id), slaBusiness.MaxRules, full)
	if !errors.Is(err, full) || back || live() != slaBusiness.MaxRules {
		t.Fatalf("the bound restores into a full project: %v %v, %d live, want refused", back, err, live())
	}
}

// Two first changes at once to a project that never saved a rule write its
// defaults once: every change, not only one that adds, holds the project's
// rules lock from before it reads whether the project saved one. Made
// deterministic by a session that holds sla_rules against writes while the
// changes start, one after the other, until each waits on a lock: without
// the rules lock, both would read "never saved" and write them. The cases:
// two adds, and a default's change (which adds nothing) with an add after
// it. The change goes first, so it is the one that writes the defaults: had
// the add written them, the default's id would name no rule (404).
func TestTwoFirstSLAChangesAtOnceWriteTheDefaultsOnce(t *testing.T) {
	type first struct {
		action string // its audit row's
		code   int    // its answer's
		call   func(w *workWorld, urgent slaRuleRead) (int, string)
	}
	add := func(name string) first {
		return first{"project.sla.rule.added", http.StatusCreated, func(w *workWorld, _ slaRuleRead) (int, string) {
			return slaCall(w, "ana", http.MethodPost, slaController.AddRule, map[string]string{"project_uuid": w.project},
				map[string]any{"name": name, "priorities": []string{"medium"}, "target_minutes": 240})
		}}
	}
	// The Urgent default's target doubled, by the default's id.
	changeUrgent := first{"project.sla.rule.changed", http.StatusOK, func(w *workWorld, urgent slaRuleRead) (int, string) {
		return slaCall(w, "ana", http.MethodPatch, slaController.ChangeRule, map[string]string{"project_uuid": w.project, "rule_id": urgent.ID},
			map[string]any{"name": urgent.Name, "priorities": urgent.Priorities, "target_minutes": 2 * urgent.TargetMinutes})
	}}
	for _, c := range []struct {
		name    string
		firsts  [2]first
		rules   int // live after both, the two defaults among them
		minutes int // the Urgent rule's target after, in its default's
	}{
		{"two adds", [2]first{add("Bugs"), add("Outages")}, 4, 1},
		{"a default's change, then an add", [2]first{changeUrgent, add("Bugs")}, 3, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := seedWorkWorld(t)
			code, read, out := slaShow(w, "ana")
			var urgent slaRuleRead
			for _, r := range read.Rules {
				if r.Name == "Urgent" && r.Virtual {
					urgent = r
				}
			}
			if code != http.StatusOK || urgent.ID == "" || urgent.TargetMinutes <= 0 {
				t.Fatalf("the defaults: %d %s, want Urgent among them", code, out)
			}
			hold, err := w.env.PG.Begin()
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(func() { _ = hold.Rollback() }) }
			t.Cleanup(release)
			if _, err := hold.Exec(`LOCK TABLE sla_rules IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			waiting := func() int {
				t.Helper()
				var n int
				if err := w.env.PG.QueryRow(`SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
					AND (query ILIKE '%INSERT INTO sla_rules%' OR query ILIKE '%pg_advisory_xact_lock(hashtext%')`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			type answer struct {
				code int
				out  string
			}
			answers := [2]chan answer{make(chan answer, 1), make(chan answer, 1)}
			for i, f := range c.firsts {
				go func() {
					code, out := f.call(w, urgent)
					answers[i] <- answer{code, out}
				}()
				for deadline := time.Now().Add(20 * time.Second); waiting() < i+1; time.Sleep(20 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("change %d of 2 never waited on a lock: %d waiting", i+1, waiting())
					}
				}
			}
			release()
			for i, f := range c.firsts {
				select {
				case a := <-answers[i]:
					if a.code != f.code {
						t.Errorf("change %d (%s): %d %s, want %d", i+1, f.action, a.code, a.out, f.code)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("change %d (%s) never answered", i+1, f.action)
				}
			}
			var all, defaults, urgents int
			if err := w.env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE name IN ('Urgent', 'High')),
				count(*) FILTER (WHERE name = 'Urgent' AND target_minutes = $2)
				FROM sla_rules WHERE project_uuid = $1 AND deleted_at IS NULL`, w.project, c.minutes*urgent.TargetMinutes).Scan(&all, &defaults, &urgents); err != nil {
				t.Fatal(err)
			}
			if all != c.rules || defaults != 2 || urgents != 1 {
				t.Fatalf("two first changes at once left %d rules, %d of them defaults, %d Urgent at %d minutes: want %d, 2 and 1",
					all, defaults, urgents, c.minutes*urgent.TargetMinutes, c.rules)
			}
			for _, action := range []string{"project.sla.rule.added", "project.sla.rule.changed"} {
				want := 0
				for _, f := range c.firsts {
					if f.action == action {
						want++
					}
				}
				if n := slaAudits(w, action); n != want {
					t.Fatalf("left %d %s rows, want %d", n, action, want)
				}
			}
		})
	}
}

// slaTimer is a task's timer row.
type slaTimer struct {
	state, endedReason, ruleID string
	started                    time.Time
	due, met                   sql.NullTime
	rows                       int
}

// slaTimerOf is the task's timer row, and whether it has one.
func slaTimerOf(w *workWorld, task string) (slaTimer, bool) {
	w.t.Helper()
	var tm slaTimer
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM task_slas WHERE task_uuid = $1`, task).Scan(&tm.rows); err != nil {
		w.t.Fatal(err)
	}
	if tm.rows == 0 {
		return tm, false
	}
	if err := w.env.PG.QueryRow(`SELECT state, COALESCE(ended_reason, ''), COALESCE(rule_id::text, ''), started_at, due_at, met_at
		FROM task_slas WHERE task_uuid = $1`, task).Scan(&tm.state, &tm.endedReason, &tm.ruleID, &tm.started, &tm.due, &tm.met); err != nil {
		w.t.Fatal(err)
	}
	return tm, true
}

// slaEvents is the task's events in the outbox, for a failure's message.
func slaEvents(w *workWorld, task string) string {
	rows, err := w.env.PG.Query(`SELECT kind, state, COALESCE(last_error, '') FROM outbox WHERE key = $1 ORDER BY id`, "task:"+task)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, state, last string
		_ = rows.Scan(&kind, &state, &last)
		out = append(out, kind+"/"+state+" "+last)
	}
	return strings.Join(out, "; ")
}

// slaMake has Ana make a task in the project through the route, its changes
// delivered.
func slaMake(w *workWorld, name, priority string) string {
	w.t.Helper()
	code, out := slaCall(w, "ana", http.MethodPost, taskController.CreateTask, nil,
		map[string]any{"task_project_uuid": w.project, "task_name": name, "task_priority": priority})
	var made struct {
		Data struct {
			Task string `json:"task_uuid"`
		} `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &made) != nil || made.Data.Task == "" {
		w.t.Fatalf("make %q: %d %s", name, code, out)
	}
	integration.Settle(w.t)
	return made.Data.Task
}

// slaChange has Ana change a task through the route h, its changes delivered.
func slaChange(w *workWorld, what string, h http.HandlerFunc, body map[string]any) {
	w.t.Helper()
	if code, out := slaCall(w, "ana", http.MethodPost, h, nil, body); code != http.StatusOK {
		w.t.Fatalf("%s: %d %s", what, code, out)
	}
	integration.Settle(w.t)
}

// slaDone has Ana mark the task done.
func slaDone(w *workWorld, task string) {
	w.t.Helper()
	slaChange(w, "done", taskController.UpdateTaskStatus, map[string]any{"task_uuid": task, "task_project_uuid": w.project, "task_status": "done"})
}

func TestSLATimersRunOnTheProjectsBusinessClock(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))
	ctx := context.Background()
	p := map[string]string{"project_uuid": w.project}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	uidOf := func(task string) string {
		t.Helper()
		var got struct {
			T []struct {
				UID string `json:"uid"`
			} `json:"t"`
		}
		raw := w.dg.Query(t, `{ t(func: eq(task_uuid, "`+task+`")) { uid } }`)
		if err := json.Unmarshal(raw, &got); err != nil || len(got.T) != 1 {
			t.Fatalf("task %s in the graph: %s %v", task, raw, err)
		}
		return got.T[0].UID
	}
	// redeliver hands the task's creation, the same entry of the log, to the
	// timers' sink once more, as the Outbox does when a delivery is retried.
	redeliver := func(task string) {
		t.Helper()
		var raw []byte
		var actor sql.NullString
		if err := w.env.PG.QueryRow(`SELECT entities, actor::text FROM outbox WHERE key = $1 AND kind = $2 ORDER BY id LIMIT 1`,
			"task:"+task, workFeed.KindCreated).Scan(&raw, &actor); err != nil {
			t.Fatalf("the creation event of %s: %v", task, err)
		}
		entities := map[string]string{}
		if err := json.Unmarshal(raw, &entities); err != nil || entities["sequence"] == "" {
			t.Fatalf("the creation event's entities: %s %v", raw, err)
		}
		e := outboxBusiness.Entry{Topic: workFeed.Topic, Kind: workFeed.KindCreated, Key: "task:" + task, Entities: entities, Sinks: []string{"task.sla"}}
		if actor.Valid {
			e.Actor = actor.String
		}
		if err := outboxBusiness.Enqueue(ctx, e); err != nil {
			t.Fatal(err)
		}
		integration.Settle(t)
	}

	// The project's week: Monday to Friday, all day, in India.
	week := map[string]any{"hours": map[string]any{"days": []int{1, 2, 3, 4, 5}, "start": "00:00", "end": "24:00", "tz": "Asia/Kolkata"}}
	if code, out := slaCall(w, "ana", http.MethodPut, slaController.SetHours, p, week); code != http.StatusOK {
		t.Fatalf("set the week: %d %s", code, out)
	}

	// An urgent task made while the project's SLAs are off gets no timer.
	first := slaMake(w, "Site is down", "urgent")
	if tm, found := slaTimerOf(w, first); found {
		t.Fatalf("a task made with SLAs off has a timer: %+v", tm)
	}
	// It was made on a Friday at 18:00 in India, as the graph says.
	friday := time.Now().In(ist).AddDate(0, 0, -14)
	for friday.Weekday() != time.Friday {
		friday = friday.AddDate(0, 0, -1)
	}
	friday = time.Date(friday.Year(), friday.Month(), friday.Day(), 18, 0, 0, 0, ist)
	monday := time.Date(friday.Year(), friday.Month(), friday.Day()+3, 18, 0, 0, 0, ist)
	w.dg.Mutate(t, []map[string]any{{"uid": uidOf(first), "task_created_at": friday.Format(time.RFC3339)}})

	// SLAs on: its creation, delivered, starts its timer at its making, due a
	// business day later: the next Monday at 18:00 in India, under the default.
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsSLA.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project},
		json.RawMessage("true"), policyBusiness.Changer{UserID: w.people["ana"], Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if code, read, out := slaShow(w, "ben"); code != http.StatusOK || !read.On {
		t.Fatalf("after switching SLAs on: %d %s, want on", code, out)
	}
	redeliver(first)
	tm, found := slaTimerOf(w, first)
	if !found || tm.state != "running" || !tm.started.Equal(friday) || !tm.due.Valid || !tm.due.Time.Equal(monday) || tm.ruleID != "" {
		t.Fatalf("an urgent task made %s: %+v (found %v), want running from then, due %s, under the default; events %s",
			friday, tm, found, monday, slaEvents(w, first))
	}
	// Delivered again: still one timer, with the same start and due.
	redeliver(first)
	if again, _ := slaTimerOf(w, first); again.rows != 1 || !again.started.Equal(tm.started) || !again.due.Time.Equal(tm.due.Time) || again.state != "running" {
		t.Fatalf("the same change delivered twice: %+v, want one row as %+v", again, tm)
	}

	// Made Low, it ends, kept, with its start.
	slaChange(w, "low", taskController.UpdateTaskPriority, map[string]any{"task_uuid": first, "task_priority": "low", "task_project_uuid": w.project})
	if low, _ := slaTimerOf(w, first); low.rows != 1 || low.state != "ended" || low.endedReason != "unmatched" || !low.started.Equal(friday) {
		t.Fatalf("made low: %+v, want ended (unmatched), kept from %s; events %s", low, friday, slaEvents(w, first))
	}

	// Done before its due: met.
	inTime := slaMake(w, "Login fails", "urgent")
	if run, _ := slaTimerOf(w, inTime); run.rows != 1 || run.state != "running" {
		t.Fatalf("an urgent task made with SLAs on: %+v, want running; events %s", run, slaEvents(w, inTime))
	}
	slaDone(w, inTime)
	if met, _ := slaTimerOf(w, inTime); met.state != "met" || !met.met.Valid || !met.due.Valid || met.met.Time.After(met.due.Time) {
		t.Fatalf("done in time: %+v, want met; events %s", met, slaEvents(w, inTime))
	}

	// Done after its due (its timer started a month ago, the start the sink
	// keeps): missed.
	late := slaMake(w, "Checkout errors", "urgent")
	if run, _ := slaTimerOf(w, late); run.state != "running" {
		t.Fatalf("an urgent task made with SLAs on: %+v, want running; events %s", run, slaEvents(w, late))
	}
	w.exec(`UPDATE task_slas SET started_at = started_at - interval '30 days' WHERE task_uuid = $1`, late)
	slaDone(w, late)
	if missed, _ := slaTimerOf(w, late); missed.state != "missed" || !missed.met.Valid || !missed.due.Valid || !missed.met.Time.After(missed.due.Time) {
		t.Fatalf("done late: %+v, want missed; events %s", missed, slaEvents(w, late))
	}

	// Its state rides its members' reads only. Eve, outside the project, is
	// its assignee, so she reads it; Gus is a guest with a member's place.
	open := slaMake(w, "Payments slow", "urgent")
	slaGuest(w)
	eve := w.account("eve").UserDgraphInfo.Uid
	openUID := uidOf(open)
	w.dg.Mutate(t, []map[string]any{{"uid": openUID, "task_assignee": map[string]any{"uid": eve}}, {"uid": eve, "user_tasks": []map[string]any{{"uid": openUID}}}})
	read := func(who string) (int, string) {
		return slaCall(w, who, http.MethodGet, taskController.GetTaskInfo, map[string]string{"task_uuid": open}, nil)
	}
	// stateOf is the task's task_sla state as who reads it, "" for none.
	stateOf := func(who string) (string, int, string) {
		code, out := read(who)
		var got struct {
			Data struct {
				SLA *struct {
					State string     `json:"state"`
					DueAt *time.Time `json:"due_at"`
				} `json:"task_sla"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil || got.Data.SLA == nil || got.Data.SLA.DueAt == nil {
			return "", code, out
		}
		return got.Data.SLA.State, code, out
	}
	for _, who := range []string{"ana", "ben"} {
		if state, code, out := stateOf(who); state != "on_track" {
			t.Errorf("%s, in the project, reads the task: %d %s, want its task_sla on track with a due", who, code, out)
		}
	}
	for _, who := range []string{"eve", "gus"} {
		if code, out := read(who); code != http.StatusOK || strings.Contains(out, `"task_sla"`) {
			t.Errorf("%s reads the task: %d %s, want it with no task_sla", who, code, out)
		}
	}
	if code, _, out := slaShow(w, "gus"); code != http.StatusForbidden {
		t.Errorf("the guest reads the project's SLAs: %d %s, want 403", code, out)
	}
	// Past its risk time, before its due: at risk, as its members read it.
	w.exec(`UPDATE task_slas SET risk_at = NOW() - interval '1 minute' WHERE task_uuid = $1`, open)
	if state, code, out := stateOf("ben"); state != "at_risk" {
		t.Errorf("a member reads the task past its risk time: %d %s, want its task_sla at risk", code, out)
	}

	// Removing the project's only rule for urgent work keeps it, removed,
	// and a new urgent task gets no running timer.
	code, settings, out := slaShow(w, "ana")
	urgent := ""
	for _, r := range settings.Rules {
		if len(r.Priorities) == 1 && r.Priorities[0] == "urgent" {
			urgent = r.ID
		}
	}
	if code != http.StatusOK || urgent == "" {
		t.Fatalf("the urgent rule: %d %s", code, out)
	}
	if code, out := slaCall(w, "ana", http.MethodDelete, slaController.RemoveRule, map[string]string{"project_uuid": w.project, "rule_id": urgent}, nil); code != http.StatusOK {
		t.Fatalf("remove the urgent rule: %d %s", code, out)
	}
	var kept, live int
	if err := w.env.PG.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NOT NULL AND deleted_by = $2), count(*) FILTER (WHERE deleted_at IS NULL)
		FROM sla_rules WHERE project_uuid = $1 AND 'urgent' = ANY(priorities)`, w.project, w.people["ana"]).Scan(&kept, &live); err != nil || kept != 1 || live != 0 {
		t.Fatalf("urgent rules after the removal: %d kept removed, %d live (%v), want 1 and 0", kept, live, err)
	}
	after := slaMake(w, "Search broken", "urgent")
	if tm, found := slaTimerOf(w, after); found && tm.state == "running" {
		t.Fatalf("an urgent task after its only rule was removed: %+v, want no running timer", tm)
	}

	// A rule for forms only: a visitor's task, filed through the project's
	// public form (business/Form Submit), runs on it as from a form; a task
	// made in the app with the same priority does not.
	code, out = slaCall(w, "ana", http.MethodPost, slaController.AddRule, p,
		map[string]any{"name": "From forms", "sources": []string{"form"}, "target_minutes": 120, "clock": "calendar"})
	var forms struct {
		Data slaRuleRead `json:"data"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(out), &forms) != nil || forms.Data.ID == "" {
		t.Fatalf("add a rule for forms: %d %s", code, out)
	}
	form, err := formBusiness.Save(ctx, uuid.MustParse(w.project), formBusiness.Input{Title: "Requests", TitleField: "what", Priority: "low", Active: true,
		Fields: []formBusiness.Field{{Id: "what", Label: "What do you need?", Type: formBusiness.ShortText, Required: true}}}, w.people["ana"])
	if err != nil {
		t.Fatalf("save the form: %v", err)
	}
	if err := formBusiness.Submit(ctx, form.Token, map[string]any{"what": "The export is empty"}, formula.NumberSigns{}, "", "", time.Now()); err != nil {
		t.Fatalf("submit the form: %v", err)
	}
	integration.Settle(t)
	var filed int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM task_slas WHERE project_uuid = $1 AND rule_id = $2 AND source = 'form' AND state = 'running'`,
		w.project, forms.Data.ID).Scan(&filed); err != nil || filed != 1 {
		t.Fatalf("a task filed through the form: %d running from a form under the rule for forms (%v), want 1", filed, err)
	}
	if tm, found := slaTimerOf(w, slaMake(w, "Typo on the page", "low")); found && tm.state == "running" {
		t.Fatalf("a low task made in the app runs on the rule for forms: %+v", tm)
	}
}

// A restricted task is timed and ended as any other (V-1016). The timers'
// sink runs on the Outbox's context, which names no reader, and a timer is
// told to nobody, so it reads its task as the system; SLA state is still laid
// per reader. Driven through the routes and the Outbox's delivery only:
// nothing here writes a timer.
func TestARestrictedTasksTimerRunsAndEndsThroughTheSink(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))
	if _, err := policyBusiness.Admin.SetIn(context.Background(), policyBusiness.ProjectsSLA.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project},
		json.RawMessage("true"), policyBusiness.Changer{UserID: w.people["ana"], Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	// restrict has Ana, an admin of the project, keep the task to those its
	// restriction lets in, through the route.
	restrict := func(task string) {
		t.Helper()
		if code, out := slaCall(w, "ana", http.MethodPut, taskRestrictionController.SetRestriction, map[string]string{"task_ref": task},
			map[string]any{"restricted": true, "people": []string{}}); code != http.StatusOK {
			t.Fatalf("restrict %s: %d %s", task, code, out)
		}
		integration.Settle(t)
		var live int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM task_restrictions WHERE task_id = $1 AND deleted_at IS NULL`, task).Scan(&live); err != nil || live != 1 {
			t.Fatalf("the restriction of %s: %d live (%v), want 1", task, live, err)
		}
	}

	// Timed, then restricted: done in time, its timer is met.
	vault := slaMake(w, "Rotate the vault keys", "urgent")
	if tm, _ := slaTimerOf(w, vault); tm.state != "running" {
		t.Fatalf("an urgent task: %+v, want running; events %s", tm, slaEvents(w, vault))
	}
	restrict(vault)
	slaDone(w, vault)
	if tm, _ := slaTimerOf(w, vault); tm.rows != 1 || tm.state != "met" || !tm.met.Valid {
		t.Errorf("a restricted task done in time: %+v, want met; events %s", tm, slaEvents(w, vault))
	}

	// Restricted before it was timed: made urgent, its timer runs; done, met.
	ledger := slaMake(w, "Reconcile the ledger", "low")
	if tm, found := slaTimerOf(w, ledger); found && tm.state == "running" {
		t.Fatalf("a low task: %+v, want no running timer", tm)
	}
	restrict(ledger)
	slaChange(w, "urgent", taskController.UpdateTaskPriority, map[string]any{"task_uuid": ledger, "task_priority": "urgent", "task_project_uuid": w.project})
	if tm, found := slaTimerOf(w, ledger); !found || tm.state != "running" {
		t.Fatalf("a restricted task made urgent: %+v (found %v), want running; events %s", tm, found, slaEvents(w, ledger))
	}
	slaDone(w, ledger)
	if tm, _ := slaTimerOf(w, ledger); tm.rows != 1 || tm.state != "met" || !tm.met.Valid {
		t.Errorf("a restricted task made urgent, then done: %+v, want met; events %s", tm, slaEvents(w, ledger))
	}
}

func TestSLATimersMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "596_sla_timers."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	// What 596 makes: its two tables, their four indexes beyond the keys,
	// the project's work week and zone.
	made := func() (tables, indexes, columns int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT
			(SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('sla_rules', 'task_slas')),
			(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname IN ('idx_sla_rules_project', 'idx_task_slas_running', 'idx_fk_task_slas_project', 'idx_fk_task_slas_rule')),
			(SELECT count(*) FROM information_schema.columns WHERE table_name = 'project_settings' AND column_name IN ('work_hours', 'work_zone'))`).
			Scan(&tables, &indexes, &columns); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i, step := range []struct {
		name, sql string
		want      int
	}{{"up", up, 1}, {"down", down, 0}, {"up", up, 1}, {"down", down, 0}, {"up", up, 1}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.name, err)
		}
		tables, indexes, columns := made()
		if tables != 2*step.want || indexes != 4*step.want || columns != 2*step.want {
			t.Errorf("step %d (%s): %d tables, %d indexes, %d columns; want %d, %d, %d",
				i, step.name, tables, indexes, columns, 2*step.want, 4*step.want, 2*step.want)
		}
	}
	// After the last up, a rule and its timer can be written as the app does.
	user, team, project, task := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rule := uuid.New()
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ('%s', 'sla@example.test', 'sla', 'SLA', NOW(), NOW())`, user),
		fmt.Sprintf(`INSERT INTO teams (id, team_name, created_by) VALUES ('%s', 'SLA', '%s')`, team, user),
		fmt.Sprintf(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ('%s', 'SLA', '%s', '%s')`, project, team, user),
		fmt.Sprintf(`INSERT INTO sla_rules (id, project_uuid, name, priorities, target_minutes, created_by) VALUES ('%s', '%s', 'Urgent', '{urgent}', 1440, '%s')`, rule, project, user),
		fmt.Sprintf(`INSERT INTO task_slas (task_uuid, project_uuid, rule_id, started_at, due_at, state) VALUES ('%s', '%s', '%s', NOW(), NOW() + interval '1 day', 'running')`, task, project, rule),
	} {
		if _, err := env.PG.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO task_slas (task_uuid, project_uuid, started_at, state) VALUES ($1, $2, NOW(), 'paused')`, uuid.New(), project); err == nil {
		t.Errorf("a timer in a state that isn't one was written")
	}
}
