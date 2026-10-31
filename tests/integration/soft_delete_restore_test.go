//go:build integration
// +build integration

package integration_test

// V-78 (the owner: "all the deletes were soft delete"): a project's custom
// field, status, cycle, milestone and form, a table's form, a person's time
// entry and a draft invoice were deleted for good. Each now goes through its
// real delete route and stays: its row is kept, marked deleted, it is gone
// from every read, and what hangs off it (a field's task values, a cycle's or
// a milestone's task links, a form's submissions) is kept. The one restore
// route (POST /restore/{kind}/{id}) brings it back, with all of that, to
// whoever its delete allows and to nobody else.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^(TestEveryDeletedProjectPartComesBackWhole)$'

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	milestoneBusiness "github.com/akashc777/OneCamp/business/Milestone"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	invoiceController "github.com/akashc777/OneCamp/controllers/Invoice"
	milestoneController "github.com/akashc777/OneCamp/controllers/Milestone"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	invoiceModel "github.com/akashc777/OneCamp/models/postgres/Invoice"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	fieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	statusModel "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	timeModel "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// deletedPart is one kind's delete and how to see it, for the walk below.
type deletedPart struct {
	kind, table, id string
	// by deletes it and may restore it; refused may not restore it.
	by      *userModels.UserInfo
	refused []*userModels.UserInfo
	del     func(who *userModels.UserInfo) *httptest.ResponseRecorder
	// shown is whether the reads that list it (and its public link) show it.
	shown func() bool
	// kept counts what hangs off it: values, task links, submissions.
	kept func() int
	// back, where its delete points what pointed at it elsewhere (a GitHub
	// automation rule's target), is that state, which its restore puts back.
	back func() string
	// taken, where a live one can take its place while it's deleted (a field
	// of its name, a cycle on its dates), makes one and answers the words its
	// restore is then refused with, and how to make way as they say.
	taken func(t *testing.T) (words string, makeWay func())
	// stops, where its restore can stop partway (a status's: a store that
	// keeps a reference to it fails once), makes the next restore stop there
	// and answers the words of its retry-later.
	stops func() (words string)
}

func TestEveryDeletedProjectPartComesBackWhole(t *testing.T) {
	ctx := context.Background()
	env, dg := calendarStores(t)
	integration.StubMqttClient(t)
	w := seedGitHubWorkspace(t, env, dg) // Website: ana its admin, ben a member; cy outside it
	project := uuid.MustParse(w.project)
	person := func(name string) *userModels.UserInfo {
		u := w.as(name).Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		return &u
	}
	ana, ben, cy := person("ana"), person("ben"), person("cy")
	tasks := []uuid.UUID{uuid.MustParse(w.task), uuid.MustParse(w.other)}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	params := func(kv ...string) map[string]string {
		m := map[string]string{"project_uuid": w.project}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	post := func(h http.HandlerFunc, who *userModels.UserInfo, body string, kv ...string) *httptest.ResponseRecorder {
		return call(t, h, http.MethodPost, "/", who, body, params(kv...))
	}

	// A field, set on two tasks.
	anaNode, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people["ana"].String())
	if err != nil || anaNode == nil {
		t.Fatalf("ana's node: %v", err)
	}
	field, err := taskFieldBusiness.Create(ctx, project, taskFieldBusiness.Input{Name: "Budget", Type: taskFieldBusiness.TypeNumber}, w.people["ana"], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if _, err := taskFieldBusiness.SetValue(ctx, &dgraphStruct.DgraphTask{Uuid: task.String()}, field, json.RawMessage(`42`), anaNode, w.people["ana"]); err != nil {
			t.Fatal(err)
		}
	}
	// A status, a cycle and a milestone with the two tasks, a form with a
	// submission, ben's time entry and a draft invoice.
	status, err := taskStatusBusiness.Create(ctx, project, w.people["ana"], taskStatusBusiness.Input{Name: "QA", Category: "inReview"})
	if err != nil {
		t.Fatal(err)
	}
	// A task in QA, as someone moved it there.
	var node struct {
		T []struct {
			UID    string `json:"uid"`
			Custom string `json:"task_custom_status"`
		} `json:"t"`
	}
	taskNode := func() string {
		node.T = nil
		if err := json.Unmarshal(dg.Query(t, `{ t(func: eq(task_uuid, "`+w.task+`")) { uid task_custom_status } }`), &node); err != nil || len(node.T) != 1 {
			t.Fatalf("the task's node: %+v %v", node, err)
		}
		return node.T[0].Custom
	}
	taskNode()
	dg.Mutate(t, map[string]any{"uid": node.T[0].UID, "task_status": "inReview", "task_custom_status": status.ID.String(), "task_custom_status_name": status.Name})
	// A store that keeps a reference to QA, as the rules do, and fails once
	// when stopped: QA's restore then stops partway, as when the graph or a
	// store is away, and the next restore must finish it, over the route.
	probe, stop := status.ID.String(), false
	taskStatusBusiness.OnRepoint(func(_ context.Context, r taskStatusBusiness.Repoint) ([]string, error) {
		if r.ProjectID != project || !r.Matches(probe) || !r.Allows("probe") {
			return nil, nil
		}
		if stop {
			stop = false
			return nil, errors.New("the probe's store is away")
		}
		probe = r.New
		return []string{"probe"}, nil
	})
	// The repository's rules: a merged pull request's task goes to QA, an
	// approved one's to In review, where QA's delete sends its tasks.
	w.exec(`UPDATE github_links SET automation_rules = $1 WHERE project_id = $2`, `{"approved":"inReview","pr_merged":"`+status.ID.String()+`"}`, w.project)
	now := time.Now()
	cycle, err := cycleModel.Create(project, "Sprint 1", now.Add(-24*time.Hour), now.Add(13*24*time.Hour), w.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cycleModel.SetTasks(ctx, tasks, &cycle.Id); err != nil {
		t.Fatal(err)
	}
	ms, err := milestoneBusiness.Create(ctx, project, milestoneBusiness.Input{Name: "Beta"}, w.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := milestoneModel.SetTasks(ctx, tasks, &ms.Id); err != nil {
		t.Fatal(err)
	}
	var form, token string
	if err := env.PG.QueryRow(`INSERT INTO project_forms (project_uuid, token, title, fields, created_by) VALUES ($1, $2, 'Bugs', '[]', $3) RETURNING id`,
		project, "tok-"+uuid.NewString(), w.people["ana"]).Scan(&form); err != nil {
		t.Fatal(err)
	}
	_ = env.PG.QueryRow(`SELECT token FROM project_forms WHERE id = $1`, form).Scan(&token)
	if _, err := formModel.Record(ctx, uuid.MustParse(form), &tasks[0], nil, json.RawMessage(`{"title":"It breaks"}`)); err != nil {
		t.Fatal(err)
	}
	ended := now.Add(-time.Hour)
	entry, err := timeModel.Add(timeModel.Entry{TaskUUID: tasks[0], ProjectUUID: project, UserID: w.people["ben"], StartedAt: now.Add(-2 * time.Hour), EndedAt: &ended, Billable: true})
	if err != nil {
		t.Fatal(err)
	}
	invoice := uuid.New()
	w.exec(`INSERT INTO invoices (id, project_id, number, issued_on, due_on, currency, subtotal_cents, tax_cents, total_cents, created_by)
		VALUES ($1, $2, 'INV-0001', CURRENT_DATE, CURRENT_DATE + 30, 'USD', 1000, 0, 1000, $3)`, invoice, project, w.people["ana"])
	// A table of ana's, open to the workspace, with a form and a submission.
	table, err := tableBusiness.CreateTable(ctx, tableBusiness.TableInput{Name: "Leads", Visibility: tableModel.VisibilityWorkspace}, tableBusiness.Actor{UserID: w.people["ana"]})
	if err != nil {
		t.Fatal(err)
	}
	var tableForm string
	if err := env.PG.QueryRow(`INSERT INTO project_forms (target_table, token, title, fields, created_by) VALUES ($1, $2, 'Sign-up', '[]', $3) RETURNING id`,
		table.Id, "tok-"+uuid.NewString(), w.people["ana"]).Scan(&tableForm); err != nil {
		t.Fatal(err)
	}
	if _, err := formModel.Record(ctx, uuid.MustParse(tableForm), nil, nil, json.RawMessage(`{"company":"Acme"}`)); err != nil {
		t.Fatal(err)
	}

	parts := []deletedPart{
		{kind: "field", table: "task_fields", id: field.ID.String(), by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(taskFieldController.DeleteField, who, "", "field_id", field.ID.String())
			},
			shown: func() bool {
				fields, _ := taskFieldBusiness.List(ctx, project)
				values, _ := fieldModel.ValuesFor(ctx, tasks)
				_, valued := values[w.task][field.ID.String()]
				for _, f := range fields {
					if f.ID == field.ID {
						return valued
					}
				}
				return false
			},
			kept: func() int { return count(`SELECT COUNT(*) FROM task_field_values WHERE field_id = $1`, field.ID) },
			taken: func(t *testing.T) (string, func()) {
				f, err := taskFieldBusiness.Create(ctx, project, taskFieldBusiness.Input{Name: "Budget", Type: taskFieldBusiness.TypeNumber}, w.people["ana"], "")
				if err != nil {
					t.Fatalf("a field of its name made since: %v", err)
				}
				return "This project already has a field with that name. Rename that one, then restore this field.", func() {
					if _, _, err := taskFieldBusiness.Update(ctx, project, f.ID, taskFieldBusiness.Input{Name: "Budget (new)", Type: taskFieldBusiness.TypeNumber}); err != nil {
						t.Fatalf("renaming that one: %v", err)
					}
				}
			}},
		{kind: "status", table: "task_statuses", id: status.ID.String(), by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(taskStatusController.Delete, who, `{"move_to":""}`, "status_id", status.ID.String())
			},
			shown: func() bool { s, _ := statusModel.List(ctx, project); return len(s) == 1 },
			kept:  func() int { return 0 },
			back: func() string {
				var raw []byte
				if err := env.PG.QueryRow(`SELECT automation_rules FROM github_links WHERE project_id = $1`, project).Scan(&raw); err != nil {
					t.Fatalf("the rules: %v", err)
				}
				var rules map[string]string
				if err := json.Unmarshal(raw, &rules); err != nil {
					t.Fatalf("the rules %s: %v", raw, err)
				}
				var moving bool
				if err := env.PG.QueryRow(`SELECT deleted_moves IS NOT NULL FROM task_statuses WHERE id = $1`, status.ID).Scan(&moving); err != nil {
					t.Fatalf("its record of what its delete moved: %v", err)
				}
				b, _ := json.Marshal(map[string]any{"rules": rules, "task": taskNode(), "probe": probe, "moving": moving})
				return string(b)
			},
			stops: func() string {
				stop = true
				return "The status is back, but its restore didn't finish. Try again in a moment."
			}},
		{kind: "cycle", table: "project_cycles", id: cycle.Id.String(), by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(cycleController.DeleteCycle, who, "", "cycle_id", cycle.Id.String())
			},
			shown: func() bool { c, _ := cycleModel.List(project); return len(c) == 1 },
			kept:  func() int { return count(`SELECT COUNT(*) FROM task_cycles WHERE cycle_id = $1`, cycle.Id) },
			taken: func(t *testing.T) (string, func()) {
				c, err := cycleModel.Create(project, "Sprint 2", cycle.StartsAt, cycle.EndsAt, w.people["ana"])
				if err != nil {
					t.Fatalf("a cycle on its dates made since: %v", err)
				}
				return "Sprint 2, made since, covers some of its dates, and cycles can't overlap. Delete that cycle first to bring this one back.", func() {
					if rec := post(cycleController.DeleteCycle, ana, "", "cycle_id", c.Id.String()); rec.Code != http.StatusOK {
						t.Fatalf("deleting that one: %d %q", rec.Code, msgOf(rec))
					}
				}
			}},
		{kind: "milestone", table: "milestones", id: ms.Id.String(), by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(milestoneController.DeleteMilestone, who, "", "milestone_id", ms.Id.String())
			},
			shown: func() bool { in, _ := milestoneModel.In(ctx, []uuid.UUID{project}); return len(in[project]) == 1 },
			kept: func() int {
				return count(`SELECT (SELECT COUNT(*) FROM task_milestones WHERE milestone_id = $1) + (SELECT COUNT(*) FROM milestone_projects WHERE milestone_id = $1)`, ms.Id)
			}},
		{kind: "form", table: "project_forms", id: form, by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(formController.DeleteForm, who, "", "form_id", form)
			},
			shown: func() bool {
				listed, _ := formModel.List(ctx, project)
				_, linkErr := formModel.ByToken(ctx, token)
				return len(listed) == 1 && linkErr == nil
			},
			kept: func() int { return count(`SELECT COUNT(*) FROM form_submissions WHERE form_id = $1`, form) }},
		{kind: "form", table: "project_forms", id: tableForm, by: ana, refused: []*userModels.UserInfo{ben},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return call(t, formController.DeleteTableForm, http.MethodPost, "/", who, "", map[string]string{"id": table.Id.String(), "form_id": tableForm})
			},
			shown: func() bool { listed, _ := formModel.ListOfTable(ctx, table.Id); return len(listed) == 1 },
			kept:  func() int { return count(`SELECT COUNT(*) FROM form_submissions WHERE form_id = $1`, tableForm) }},
		{kind: "time_entry", table: "task_time_entries", id: entry.Id.String(), by: ben, refused: []*userModels.UserInfo{ana},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(timeEntryController.DeleteTime, who, "", "entry_id", entry.Id.String())
			},
			shown: func() bool {
				onTask, _ := timeModel.ForTask(tasks[0])
				billed, _ := timeModel.ForProject(project, now.Add(-24*time.Hour), now.Add(time.Hour))
				// The project report's weekly hours (business/Project/report.go).
				weeks, _ := timeModel.SecondsByWeek([]uuid.UUID{project}, now.Add(-14*24*time.Hour), now, "UTC")
				var reported int64
				for _, wk := range weeks {
					reported += wk.Seconds
				}
				return len(onTask) == 1 && len(billed) == 1 && reported > 0
			},
			kept: func() int { return 0 }},
		{kind: "invoice", table: "invoices", id: invoice.String(), by: ana, refused: []*userModels.UserInfo{ben, cy},
			del: func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return post(invoiceController.DeleteInvoice, who, "", "invoice_id", invoice.String())
			},
			shown: func() bool { listed, _ := invoiceModel.List(ctx, project); return len(listed) == 1 },
			kept:  func() int { return 0 }},
	}

	for _, p := range parts {
		t.Run(p.kind+" "+p.table, func(t *testing.T) {
			if !p.shown() {
				t.Fatal("not shown before its delete")
			}
			before := p.kept()
			var was string
			if p.back != nil {
				was = p.back()
			}
			if rec := p.del(p.by); rec.Code != http.StatusOK {
				t.Fatalf("delete: %d %q", rec.Code, msgOf(rec))
			}
			// The row stays, with what hangs off it, and every read leaves it out.
			if n := count(`SELECT COUNT(*) FROM `+p.table+` WHERE id = $1`, p.id); n != 1 {
				t.Fatalf("its row is gone after the delete (%d rows): the delete is hard", n)
			}
			if got := p.kept(); got != before {
				t.Errorf("what hangs off it went from %d to %d", before, got)
			}
			if p.shown() {
				t.Errorf("still shown after its delete")
			}
			if p.back != nil && p.back() == was {
				t.Errorf("its delete pointed nothing elsewhere: %s", was)
			}
			// RESTORE{
			restore := func(who *userModels.UserInfo) *httptest.ResponseRecorder {
				return call(t, restoreController.Restore, http.MethodPost, "/restore/"+p.kind+"/"+p.id, who, "", map[string]string{"kind": p.kind, "id": p.id})
			}
			for _, who := range p.refused {
				if rec := restore(who); rec.Code == http.StatusOK {
					t.Errorf("%s restored it: %d %q", who.UserPostgresInfo.Id, rec.Code, msgOf(rec))
				}
			}
			if p.shown() {
				t.Fatal("shown after a refused restore")
			}
			// Where a live one has taken its place since, the restore says so
			// and how to make way, and the part stays deleted until then.
			if p.taken != nil {
				words, makeWay := p.taken(t)
				if rec := restore(p.by); rec.Code == http.StatusOK || msgOf(rec) != words {
					t.Errorf("a restore with its place taken: %d %q, want %q", rec.Code, msgOf(rec), words)
				}
				if n := count(`SELECT COUNT(*) FROM `+p.table+` WHERE id = $1 AND deleted_at IS NOT NULL`, p.id); n != 1 {
					t.Fatal("restored with its place taken")
				}
				makeWay()
			}
			// A restore that stops partway answers retry-later and leaves the
			// rest to do. Those refused may not finish it; the next restore by
			// whoever may restore it does.
			if p.stops != nil {
				words := p.stops()
				if rec := restore(p.by); rec.Code != http.StatusServiceUnavailable || msgOf(rec) != words {
					t.Errorf("a restore that stopped partway: %d %q, want 503 %q", rec.Code, msgOf(rec), words)
				}
				for _, who := range p.refused {
					if rec := restore(who); rec.Code == http.StatusOK {
						t.Errorf("%s finished its restore: %d %q", who.UserPostgresInfo.Id, rec.Code, msgOf(rec))
					}
				}
				if p.back() == was {
					t.Errorf("a restore that stopped partway left nothing to finish: %s", was)
				}
				// Deleted again before its restore finished: that delete keeps
				// the first one's moves beside its own, so the restore below
				// still brings all of both back.
				if rec := p.del(p.by); rec.Code != http.StatusOK {
					t.Fatalf("deleting it again before its restore finished: %d %q", rec.Code, msgOf(rec))
				}
			}
			if rec := restore(p.by); rec.Code != http.StatusOK {
				t.Fatalf("restore: %d %q", rec.Code, msgOf(rec))
			}
			if !p.shown() || p.kept() != before {
				t.Errorf("after its restore: shown %v, kept %d of %d", p.shown(), p.kept(), before)
			}
			if p.back != nil {
				if now := p.back(); now != was {
					t.Errorf("what its delete pointed elsewhere isn't back: %s, was %s", now, was)
				}
			}
			if rec := restore(p.by); rec.Code != http.StatusNotFound {
				t.Errorf("a second restore: %d %q, want 404", rec.Code, msgOf(rec))
			}
			// }RESTORE
		})
	}
}

// TestStartingACycleLeavesDeletedOnesAsTheyWere: starting a cycle ends the
// completed cycles it overlaps where it starts, but never a deleted one, which
// keeps its dates while deleted. Its restore then makes the room Start would
// have made: a completed cycle comes back ending where the one started since
// begins. One that isn't complete gives up no days: while a live cycle covers
// some of them its restore is refused (ErrOverlap) and it stays deleted, and
// once that one is deleted it comes back with its own dates.
func TestStartingACycleLeavesDeletedOnesAsTheyWere(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, by := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, by, by.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Cycles', $2)`, project, by); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	now := time.Now().UTC().Truncate(time.Second)
	endsOf := func(id uuid.UUID) time.Time {
		t.Helper()
		var ends time.Time
		if err := env.PG.QueryRow(`SELECT ends_at FROM project_cycles WHERE id = $1`, id).Scan(&ends); err != nil {
			t.Fatal(err)
		}
		return ends.UTC()
	}
	done, err := cycleModel.Create(project, "Done", now.Add(-20*day), now.Add(-6*day), by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE project_cycles SET completed_at = ends_at WHERE id = $1`, done.Id); err != nil {
		t.Fatal(err)
	}
	next, err := cycleModel.Create(project, "Next", now.Add(day), now.Add(14*day), by)
	if err != nil {
		t.Fatal(err)
	}
	if err := cycleModel.Delete(ctx, done.Id, by); err != nil {
		t.Fatal(err)
	}
	if _, err := cycleModel.Start(ctx, project, next.Id, now.Add(-10*day), now.Add(4*day)); err != nil {
		t.Fatal(err)
	}
	if got := endsOf(done.Id); !got.Equal(now.Add(-6 * day)) {
		t.Errorf("starting another changed the deleted cycle: it ends %s, not %s", got, now.Add(-6*day))
	}
	if _, err := cycleModel.Restore(ctx, done.Id); err != nil {
		t.Fatalf("the completed cycle's restore, with the one started since on its last days: %v", err)
	}
	if got := endsOf(done.Id); !got.Equal(now.Add(-10 * day)) {
		t.Errorf("the completed cycle comes back ending %s, not where the one started since begins (%s)", got, now.Add(-10*day))
	}

	later, err := cycleModel.Create(project, "Later", now.Add(30*day), now.Add(44*day), by)
	if err != nil {
		t.Fatal(err)
	}
	if err := cycleModel.Delete(ctx, later.Id, by); err != nil {
		t.Fatal(err)
	}
	over, err := cycleModel.Create(project, "Over", now.Add(28*day), now.Add(40*day), by)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := cycleModel.Restore(ctx, later.Id); ok || !errors.Is(err, cycleModel.ErrOverlap) {
		t.Fatalf("restoring one that isn't complete over a cycle made since: %v %v, want ErrOverlap", ok, err)
	}
	var deleted bool
	if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM project_cycles WHERE id = $1`, later.Id).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("a refused restore brought it back (%v)", err)
	}
	if err := cycleModel.Delete(ctx, over.Id, by); err != nil {
		t.Fatal(err)
	}
	if ok, err := cycleModel.Restore(ctx, later.Id); !ok || err != nil {
		t.Fatalf("restoring it once that one is deleted: %v %v", ok, err)
	}
	if got := endsOf(later.Id); !got.Equal(now.Add(44 * day)) {
		t.Errorf("it comes back ending %s, not its own %s", got, now.Add(44*day))
	}
}

// TestSoftDeleteMigrationUpDownUp: the soft-delete migration goes down and up
// again, twice, found by its name so a renumber at landing keeps it. Its down
// takes the deleted rows away first, as the schema before it never kept them,
// so the whole-name indexes can be built again: a live field keeps its name
// beside a deleted one until then, and stays.
func TestSoftDeleteMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_soft_delete_project_parts." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("the migration's %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	has := func(table, column string) bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	project := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO task_fields (id, project_id, name, type, deleted_at) VALUES ($1, $3, 'Budget', 'number', NULL), ($2, $3, 'Budget', 'number', NOW())`,
		uuid.New(), uuid.New(), project); err != nil {
		t.Fatal(err)
	}
	// A deleted row of every other kind, a cycle with a task in it, so each
	// DELETE in the down runs on a real row and its cascades.
	by, task := uuid.New(), uuid.New()
	gone := map[string]uuid.UUID{"task_statuses": uuid.New(), "project_cycles": uuid.New(), "milestones": uuid.New(), "task_time_entries": uuid.Nil, "project_forms": uuid.Nil, "invoices": uuid.New()}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{by, by.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Parts', $2)`, []any{project, by}},
		{`INSERT INTO tasks (id) VALUES ($1)`, []any{task}},
		{`INSERT INTO task_statuses (id, project_id, name, category, color, position, deleted_at) VALUES ($1, $2, 'Ready', 'todo', 'violet', 0, NOW())`, []any{gone["task_statuses"], project}},
		{`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at, deleted_at) VALUES ($1, $2, 1, '', NOW(), NOW() + interval '14 days', NOW())`, []any{gone["project_cycles"], project}},
		{`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, []any{task, gone["project_cycles"]}},
		{`INSERT INTO milestones (id, project_uuid, name, deleted_at) VALUES ($1, $2, 'Launch', NOW())`, []any{gone["milestones"], project}},
		{`INSERT INTO invoices (id, project_id, number, issued_on, due_on, currency, subtotal_cents, tax_cents, total_cents, created_by, deleted_at)
			VALUES ($1, $2, 'INV-0001', CURRENT_DATE, CURRENT_DATE + 30, 'USD', 1000, 0, 1000, $3, NOW())`, []any{gone["invoices"], project, by}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	for table, q := range map[string]string{
		"task_time_entries": `INSERT INTO task_time_entries (task_uuid, project_uuid, user_id, started_at, ended_at, deleted_at) VALUES ($1, $2, $3, NOW() - interval '1 hour', NOW(), NOW()) RETURNING id`,
		"project_forms":     `INSERT INTO project_forms (project_uuid, token, title, fields, created_by, deleted_at) VALUES ($2, 'tok-' || $1::text, 'Bugs', '[]', $3, NOW()) RETURNING id`,
	} {
		var id uuid.UUID
		if err := env.PG.QueryRow(q, task, project, by).Scan(&id); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		gone[table] = id
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, c := range [][2]string{{"task_fields", "deleted_at"}, {"task_statuses", "deleted_moves"}, {"invoices", "deleted_by"}, {"task_time_entries", "deleted_at"}} {
			if got := has(c[0], c[1]); got != step.want {
				t.Fatalf("step %d: %s.%s exists = %v", i, c[0], c[1], got)
			}
		}
	}
	var n int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM task_fields WHERE project_id = $1`, project).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the live Budget stays, the deleted one went with the first down: %d %v", n, err)
	}
	for table, id := range gone {
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil || n != 0 {
			t.Errorf("the deleted %s row went with the first down: %d left, %v", table, n, err)
		}
	}
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM task_cycles WHERE task_uuid = $1`, task).Scan(&n); err != nil || n != 0 {
		t.Errorf("the deleted cycle's task link went with it: %d left, %v", n, err)
	}
}
