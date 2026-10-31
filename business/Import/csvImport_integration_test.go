//go:build integration

package business

// A CSV of tasks comes across as a project, end to end: the file staged as an
// upload is, the plan, then the run. The file is Asana's export, its Name,
// Section/Column, Assignee (and Assignee Email), Due Date and Parent task
// columns, and two custom fields.
//
// The people are the case: Ana Lima is one member (a bot shares her name, and
// a bot is no one's match), matched by her name alone, so her row comes in
// unassigned, saying whom the file assigned it to, and she can't read it
// (placed.go); two members are called Bo Ray, so neither is guessed; Cyrus is
// known by his address; Dee Moss's account is deactivated, so a row naming
// her gets a placeholder, never her.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	_ "github.com/akashc777/OneCamp/business/Import/providers/csv"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const asanaLaunchCSV = "Task ID,Created At,Completed At,Last Modified,Name,Section/Column,Assignee,Assignee Email,Start Date,Due Date,Tags,Notes,Projects,Parent task,Budget (USD),Sprint\n" +
	"1001,2026-10-01,,2026-10-02,Plan the launch,Doing,Ana Lima,,,2026-11-02,,Kick-off notes,Launch,,1200,S1\n" +
	"1002,2026-10-01,,2026-10-02,Write the brief,To do,Bo Ray,,,,,,Launch,Plan the launch,,S1\n" +
	"1003,2026-10-01,2026-10-03,2026-10-03,Ship it,Done,Cyrus,cyrus@acme.test,,,,,Launch,,300.50,S2\n" +
	"1004,2026-10-01,,2026-10-02,Ask Dee,To do,Dee Moss,,,,,,Launch,,,\n"

func TestACSVOfTasksComesAcrossWithItsPeopleMatched(t *testing.T) {
	ctx := context.Background()
	env, dg := importEnv(t)
	const bucket = "csv-import-test"
	store := integration.SetupMinio(t, bucket)

	admin, ana, anaBot, bo1, bo2, cyrus, dee := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, is_bot, deleted_at, created_at, updated_at) VALUES
		($1, 'admin@acme.test', false, NULL, NOW(), NOW()), ($2, 'ana@acme.test', false, NULL, NOW(), NOW()),
		($3, 'ana-bot@bots.acme.test', true, NULL, NOW(), NOW()), ($4, 'bo@acme.test', false, NULL, NOW(), NOW()),
		($5, 'bo.ray@acme.test', false, NULL, NOW(), NOW()), ($6, 'cyrus@acme.test', false, NULL, NOW(), NOW()),
		($7, 'dee@acme.test', false, NOW(), NOW(), NOW())`, admin, ana, anaBot, bo1, bo2, cyrus, dee); err != nil {
		t.Fatal(err)
	}
	user := func(key string, id uuid.UUID, name string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": "0001-01-01T00:00:00Z"}
	}
	uids := dg.Mutate(t, []map[string]any{
		user("admin", admin, "Ada Admin"), user("ana", ana, "Ana Lima"), user("anabot", anaBot, "Ana Lima"),
		user("bo1", bo1, "Bo Ray"), user("bo2", bo2, "Bo Ray"), user("cyrus", cyrus, "Cyrus Day"), user("dee", dee, "Dee Moss"),
	})

	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderCSV, SourceWorkspaceName: "Launch",
		Source: importModels.SourceBackupCSV, Status: importModels.StatusValidating, TriggeredBy: &admin}
	key := fmt.Sprintf("imports/csv/%s/raw", job.Id)
	job.RawObjectKey = &key
	if _, err := store.PutObject(ctx, bucket, key, strings.NewReader(asanaLaunchCSV), int64(len(asanaLaunchCSV)), minio.PutObjectOptions{ContentType: "text/csv"}); err != nil {
		t.Fatal(err)
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// The plan: who each person will be, and what each column becomes.
	plan, err := BuildPlan(ctx, job.Id)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	people := map[string]importProvider.PlanPerson{}
	for _, p := range plan.People {
		people[p.Name] = p
	}
	for name, want := range map[string]importProvider.PlanPerson{
		"Ana Lima": {MatchedBy: "name", Member: "Ana Lima"},
		"Bo Ray":   {SameName: 2},
		"Cyrus":    {MatchedBy: "email", Member: "Cyrus Day"},
		"Dee Moss": {},
	} {
		got, ok := people[name]
		if !ok || got.MatchedBy != want.MatchedBy || got.Member != want.Member || got.SameName != want.SameName {
			t.Errorf("the plan's %s: %+v (listed %v), want matched_by %q, member %q, same_name %d", name, got, ok, want.MatchedBy, want.Member, want.SameName)
		}
	}
	kinds := map[string]importProvider.PlanColumn{}
	for _, c := range plan.Columns {
		kinds[c.Name] = c
	}
	if c := kinds["Budget (USD)"]; c.Kind != importProvider.FieldMoney || c.Currency != "USD" {
		t.Errorf("Budget (USD) is read as %+v, want money in USD", c)
	}
	if c := kinds["Sprint"]; c.Kind != importProvider.FieldSelect {
		t.Errorf("Sprint is read as %+v, want a select", c)
	}

	// The run.
	if err := importModels.UpdateStatus(ctx, job.Id, importModels.StatusRunning, strPtr("users"), nil); err != nil {
		t.Fatal(err)
	}
	importer := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: admin}}
	importer.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["admin"], Uuid: admin.String()}
	RunImport(ctx, job.Id, importer)
	done, err := importModels.GetJob(ctx, job.Id)
	if err != nil || done.Status != importModels.StatusCompleted {
		t.Fatalf("the run ended %+v (%v)", done, err)
	}

	made := map[string]uuid.UUID{}
	for _, entity := range []string{importModels.EntityTask, importModels.EntitySubtask} {
		m, err := importModels.IdMapOf(ctx, job.Id, entity)
		if err != nil {
			t.Fatal(err)
		}
		for src, id := range m {
			made[src[strings.LastIndex(src, ":")+1:]] = id
		}
	}
	type task struct {
		Name        string `json:"task_name"`
		Description string `json:"task_description"`
		Assignee    *struct {
			Uuid string `json:"user_uuid"`
		} `json:"task_assignee"`
		Subtasks []struct {
			Uuid string `json:"task_uuid"`
		} `json:"task_sub_tasks"`
	}
	read := func(row string) task {
		t.Helper()
		id, ok := made[row]
		if !ok {
			t.Fatalf("row %s made no task: %v", row, made)
		}
		var out struct {
			Q []task `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_name task_description task_assignee { user_uuid } task_sub_tasks { task_uuid } } }`, id)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("row %s's task %s in the graph: %+v (%v)", row, id, out, err)
		}
		return out.Q[0]
	}
	assignee := func(tk task) string {
		if tk.Assignee == nil {
			return ""
		}
		return tk.Assignee.Uuid
	}
	placeholder := func(row string, not ...uuid.UUID) {
		t.Helper()
		got := assignee(read(row))
		for _, n := range not {
			if got == n.String() {
				t.Errorf("row %s is assigned to %s, whom it must not guess", row, got)
			}
		}
		var external bool
		if err := env.PG.QueryRow(`SELECT is_external FROM users WHERE id = $1`, got).Scan(&external); err != nil || !external {
			t.Errorf("row %s is assigned to %q, not a placeholder (%v)", row, got, err)
		}
	}

	plan1001 := read("1001")
	if plan1001.Name != "Plan the launch" || assignee(plan1001) != "" ||
		!strings.HasPrefix(plan1001.Description, "<p><em>Imported from CSV: assigned to Ana Lima, not matched to a member by email.</em></p>") {
		t.Errorf("row 1001: %+v, want Plan the launch, unassigned, saying the file assigned it to Ana Lima", plan1001)
	}
	if readers, err := taskDomain.Readers(ctx, made["1001"].String(), []string{ana.String()}); err != nil || readers[ana.String()] {
		t.Errorf("Ana may read row 1001: %v (%v)", readers, err)
	}
	var unassigned string
	if err := env.PG.QueryRow(`SELECT message FROM import_errors WHERE import_id = $1 AND code = 'ASSIGNEE_NOT_MATCHED'`, job.Id).Scan(&unassigned); err != nil ||
		!strings.Contains(unassigned, "assigned to Ana Lima in CSV") {
		t.Errorf("the report's line for row 1001: %q (%v)", unassigned, err)
	}
	if len(plan1001.Subtasks) != 1 || plan1001.Subtasks[0].Uuid != made["1002"].String() {
		t.Errorf("Write the brief isn't Plan the launch's sub-task: %+v", plan1001.Subtasks)
	}
	placeholder("1002", bo1, bo2)
	if got := assignee(read("1003")); got != cyrus.String() {
		t.Errorf("row 1003 is assigned to %q, want Cyrus by his address", got)
	}
	placeholder("1004", dee)

	// Each field column is a field of its kind, with the rows' values.
	values := map[string]string{}
	rows, err := env.PG.Query(`SELECT f.name || ':' || f.type || ':' || COALESCE(f.currency, ''), v.value::text
		FROM task_field_values v JOIN task_fields f ON f.id = v.field_id WHERE v.task_uuid = $1`, made["1001"])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var field, value string
		if err := rows.Scan(&field, &value); err != nil {
			t.Fatal(err)
		}
		values[field] = value
	}
	if v, ok := values["Budget:money:USD"]; !ok || !strings.Contains(v, "1200") {
		t.Errorf("row 1001's budget: %v", values)
	}
	if _, ok := values["Sprint:select:"]; !ok {
		t.Errorf("row 1001's sprint: %v", values)
	}
}
