//go:build integration

package business

// A task list's CSV (asana-16, jira-16) against Postgres 12 with every
// migration and Dgraph with the production schema: its columns and every kind
// of field as the reader's calendar has them, a restricted task left out for
// a member it is hidden from (rows and count alike), the Manual order, an
// empty list, and a list longer than ExportMax.
// Run: PKGS=./business/Project integ_rel.sh <wt> <log> 'TestATaskListsCSV'

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

const liveCSV = "0001-01-01T00:00:00Z"

// exportedCSV is a list's file as written: the counts start was given, whether
// it began with the byte order mark, and its records.
type exportedCSV struct {
	total, rows int
	bom         bool
	records     [][]string
}

// column is the record's cell under the header named name.
func (e exportedCSV) column(t *testing.T, record []string, name string) string {
	t.Helper()
	i := slices.Index(e.records[0], name)
	if i < 0 {
		t.Fatalf("no column %q in %q", name, e.records[0])
	}
	return record[i]
}

// titles are the rows' titles in order.
func (e exportedCSV) titles(t *testing.T) []string {
	var out []string
	for _, r := range e.records[1:] {
		out = append(out, e.column(t, r, "Title"))
	}
	return out
}

func exportTaskList(t *testing.T, ctx context.Context, x TaskListExport) exportedCSV {
	t.Helper()
	var buf bytes.Buffer
	var got exportedCSV
	starts := 0
	if err := WriteTaskListCSV(ctx, x, func(total, rows int) io.Writer {
		starts++
		got.total, got.rows = total, rows
		return &buf
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if starts != 1 {
		t.Fatalf("the file was started %d times", starts)
	}
	const bom = "\xef\xbb\xbf"
	got.bom = bytes.HasPrefix(buf.Bytes(), []byte(bom))
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), bom))).ReadAll()
	if err != nil || len(records) == 0 {
		t.Fatalf("the file reads back as %d records: %v", len(records), err)
	}
	got.records = records
	return got
}

func connectCSVStores(t *testing.T, ctx context.Context) *integration.DgraphEnv {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	return integration.SetupDgraph(t)
}

func execCSV(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestATaskListsCSV(t *testing.T) {
	ctx := context.Background()
	dg := connectCSVStores(t, ctx)
	lead, maya, ben, team, project, milestone := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{lead: "lee", maya: "maya", ben: "ben"} {
		execCSV(t, ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@csv.test', $2, $2, NOW(), NOW())`, id, name)
	}
	execCSV(t, ctx, `INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Fair', $2)`, team, lead)
	execCSV(t, ctx, `INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Spring fair', $2, $3)`, project, team, lead)
	execCSV(t, ctx, `INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'SF', 4)`, project)
	execCSV(t, ctx, `INSERT INTO task_types (id, project_id, name, icon) VALUES ($1, $2, 'Milestone', 'flag')`, milestone, project)

	// The launch post (Maya's, with every field set), the venue under it, whose
	// title reads like a formula, and the pricing, restricted to Maya.
	post, venue, pricing := uuid.New(), uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{post, venue, pricing} {
		execCSV(t, ctx, `INSERT INTO tasks (id, project_id, task_number) VALUES ($1, $2, $3)`, id, project, i+1)
	}
	execCSV(t, ctx, `UPDATE tasks SET task_type_id = $1 WHERE id = $2`, milestone, post)
	person := func(key string, id uuid.UUID, name string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": liveCSV}
	}
	task := func(key string, id uuid.UUID, name, created string, more map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": name, "task_status": "todo",
			"task_deleted_at": liveCSV, "task_created_at": created}
		for k, v := range more {
			n[k] = v
		}
		return n
	}
	uids := dg.Mutate(t, []map[string]any{
		person("lead", lead, "Lee Park"), person("maya", maya, "Maya Chen"), person("ben", ben, "Ben Ito"),
		task("post", post, "Write the launch post", "2026-10-20T09:15:00Z", map[string]any{
			"task_status": "inProgress", "task_priority": "high", "task_assignee": map[string]any{"uid": "_:maya"},
			"task_start_date": "2026-11-01T23:00:00Z", "task_label": "launch, web", "task_estimate_minutes": 90,
			"task_created_by": map[string]any{"uid": "_:lead"}, "task_rank": 1.0,
			"task_description": "<p>Draft it</p><ul><li><p>copy</p></li><li><p>ads</p></li></ul>"}),
		task("venue", venue, "=Book the venue", "2026-10-21T09:00:00Z", map[string]any{"task_parent_task": map[string]any{"uid": "_:post"}, "task_rank": 2.0}),
		task("pricing", pricing, "Pricing", "2026-10-22T09:00:00Z", nil),
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Spring fair", "project_deleted_at": liveCSV,
			"project_tasks":   []map[string]any{{"uid": "_:post"}, {"uid": "_:venue"}, {"uid": "_:pricing"}},
			"project_members": []map[string]any{{"uid": "_:lead"}, {"uid": "_:maya"}, {"uid": "_:ben"}},
			"project_admins":  []map[string]any{{"uid": "_:lead"}}},
	})
	if err := restrictionModel.Restrict(ctx, pricing, project, lead, []uuid.UUID{maya}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	field := func(in taskFieldBusiness.Input) string {
		t.Helper()
		f, err := taskFieldBusiness.Create(ctx, project, in, lead, "")
		if err != nil {
			t.Fatalf("field %s: %v", in.Name, err)
		}
		set := func(raw string) {
			t.Helper()
			if _, err := taskFieldBusiness.SetValue(ctx, &dgraphStruct.DgraphTask{Uuid: post.String()}, f, json.RawMessage(raw), &dgraphStruct.DgraphUser{Uid: uids["lead"]}, lead); err != nil {
				t.Fatalf("%s = %s: %v", f.Name, raw, err)
			}
		}
		switch in.Type {
		case taskFieldBusiness.TypeText:
			set(`"=1+1"`)
		case taskFieldBusiness.TypeNumber:
			set(`-3.5`)
		case taskFieldBusiness.TypeMoney:
			set(`-250`)
		case taskFieldBusiness.TypeDate:
			set(`"2026-11-20"`)
		case taskFieldBusiness.TypeSelect:
			set(`"` + f.Options[1].ID + `"`)
		case taskFieldBusiness.TypeMultiSelect:
			set(`["` + f.Options[0].ID + `","` + f.Options[1].ID + `"]`)
		case taskFieldBusiness.TypePerson:
			set(`"` + maya.String() + `"`)
		case taskFieldBusiness.TypeCheckbox:
			set(`true`)
		case taskFieldBusiness.TypeURL:
			set(`"https://fair.example/launch"`)
		}
		return f.Name
	}
	for _, in := range []taskFieldBusiness.Input{
		{Name: "Notes", Type: taskFieldBusiness.TypeText},
		{Name: "Score", Type: taskFieldBusiness.TypeNumber},
		{Name: "Budget", Type: taskFieldBusiness.TypeMoney, Currency: "EUR"},
		{Name: "Launch", Type: taskFieldBusiness.TypeDate},
		{Name: "Channel", Type: taskFieldBusiness.TypeSelect, Options: []taskFieldBusiness.OptionInput{{Label: "Blog"}, {Label: "Email"}}},
		{Name: "Areas", Type: taskFieldBusiness.TypeMultiSelect, Options: []taskFieldBusiness.OptionInput{{Label: "Web"}, {Label: "App"}}},
		{Name: "Reviewer", Type: taskFieldBusiness.TypePerson},
		{Name: "Signed off", Type: taskFieldBusiness.TypeCheckbox},
		{Name: "Site", Type: taskFieldBusiness.TypeURL},
		{Name: "status", Type: taskFieldBusiness.TypeText},
	} {
		field(in)
	}

	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	// The list's own order when it names none: newest first.
	newest, err := dgraphquery.TaskListOrder("")
	if err != nil {
		t.Fatal(err)
	}
	as := func(id uuid.UUID, key string) (context.Context, TaskListExport) {
		return taskread.ActingFor(ctx, id.String()), TaskListExport{ProjectUUID: project.String(), ReaderUID: uids[key], Sort: newest, Location: kolkata}
	}

	t.Run("its columns, then every kind of field, as the reader's calendar has them", func(t *testing.T) {
		c, x := as(maya, "maya")
		got := exportTaskList(t, c, x)
		if !got.bom || got.total != 3 || got.rows != 3 || len(got.records) != 4 {
			t.Fatalf("bom %v, total %d, rows %d, records %d", got.bom, got.total, got.rows, len(got.records))
		}
		wantHeader := []string{"Key", "Title", "Type", "Status", "Priority", "Assignee", "Start date", "Due date", "Estimate (hours)", "Tags", "Parent", "Created", "Created by",
			"Notes", "Score", "Budget (EUR)", "Launch", "Channel", "Areas", "Reviewer", "Signed off", "Site", "status (field)", "Description", "Link"}
		if !slices.Equal(got.records[0], wantHeader) {
			t.Fatalf("header %q", got.records[0])
		}
		if titles := got.titles(t); !slices.Equal(titles, []string{"Pricing", "'=Book the venue", "Write the launch post"}) {
			t.Fatalf("newest first: %q", titles)
		}
		row := got.records[3]
		want := []string{"SF-1", "Write the launch post", "Milestone", "In progress", "High", "Maya Chen", "2026-11-02", "", "1.50", "launch, web", "", "2026-10-20 14:45", "Lee Park",
			"'=1+1", "-3.5", "-2.50", "2026-11-20", "Email", "Web, App", "Maya Chen", "Yes", "https://fair.example/launch", "'=1+1", "Draft it\n\n- copy\n- ads"}
		if !slices.Equal(row[:len(want)], want) {
			t.Errorf("the launch post\n got %q\nwant %q", row[:len(want)], want)
		}
		if link := row[len(row)-1]; !strings.HasSuffix(link, "/app/task/"+post.String()) {
			t.Errorf("its link is %q", link)
		}
		if parent := got.column(t, got.records[2], "Parent"); parent != "SF-1" {
			t.Errorf("the venue's parent is %q", parent)
		}
	})

	t.Run("a restricted task is left out for a member it is hidden from, rows and count alike", func(t *testing.T) {
		c, x := as(ben, "ben")
		got := exportTaskList(t, c, x)
		if got.total != 2 || got.rows != 2 {
			t.Errorf("total %d, rows %d", got.total, got.rows)
		}
		if titles := got.titles(t); !slices.Equal(titles, []string{"'=Book the venue", "Write the launch post"}) {
			t.Errorf("Ben reads %q", titles)
		}
	})

	t.Run("the Manual order, as the list has it", func(t *testing.T) {
		c, x := as(maya, "maya")
		x.Sort = dgraphquery.ManualOrder
		got := exportTaskList(t, c, x)
		// Never moved first (the pricing), then by rank.
		if titles := got.titles(t); !slices.Equal(titles, []string{"Pricing", "Write the launch post", "'=Book the venue"}) || got.total != 3 {
			t.Errorf("Manual: %q of %d", titles, got.total)
		}
		c, x = as(ben, "ben")
		x.Sort = dgraphquery.ManualOrder
		got = exportTaskList(t, c, x)
		if titles := got.titles(t); !slices.Equal(titles, []string{"Write the launch post", "'=Book the venue"}) || got.total != 2 {
			t.Errorf("Ben's Manual: %q of %d", titles, got.total)
		}
	})

	t.Run("an empty list is its header", func(t *testing.T) {
		for _, sort := range []string{newest, dgraphquery.ManualOrder} {
			c, x := as(maya, "maya")
			x.Filter, x.Sort = `eq(task_status, "done")`, sort
			got := exportTaskList(t, c, x)
			if !got.bom || got.total != 0 || got.rows != 0 || len(got.records) != 1 || got.records[0][0] != "Key" {
				t.Errorf("%s: bom %v, total %d, rows %d, records %q", sort, got.bom, got.total, got.rows, got.records)
			}
		}
	})
}

// A list longer than ExportMax: the file holds its first ExportMax rows, and
// start is told how many the list holds, so the app can say so.
func TestATaskListsCSVStopsAtExportMax(t *testing.T) {
	ctx := context.Background()
	dg := connectCSVStores(t, ctx)
	project := uuid.NewString()
	p := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Backlog", "project_deleted_at": liveCSV})["p"]
	const chunk = 10_000
	for from := 0; from <= ExportMax; from += chunk {
		var tasks []map[string]any
		for i := from; i < min(from+chunk, ExportMax+1); i++ {
			tasks = append(tasks, map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": fmt.Sprintf("Task %d", i),
				"task_status": "todo", "task_deleted_at": liveCSV, "task_created_at": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
		}
		dg.Mutate(t, map[string]any{"uid": p, "project_tasks": tasks})
	}
	newest, err := dgraphquery.TaskListOrder("")
	if err != nil {
		t.Fatal(err)
	}
	got := exportTaskList(t, taskread.WithSystem(ctx), TaskListExport{ProjectUUID: project, ReaderUID: p, Sort: newest, Location: time.UTC})
	if got.total != ExportMax+1 || got.rows != ExportMax || len(got.records) != ExportMax+1 {
		t.Fatalf("total %d, rows %d, records %d", got.total, got.rows, len(got.records))
	}
	if first, last := got.records[1][1], got.records[ExportMax][1]; first != fmt.Sprintf("Task %d", ExportMax) || last != "Task 1" {
		t.Errorf("newest first, the oldest left out: %q … %q", first, last)
	}
}
