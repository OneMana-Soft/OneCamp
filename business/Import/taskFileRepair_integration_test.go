//go:build integration

package business

// SF2 (CR-47 on F): imports before F linked a task's file with an edge the
// graph refused, so their tasks never listed their files. The repair links
// each file an import kept for a task, once per install. Against Postgres 12
// with every migration and Dgraph.
// Run: PKGS=./business/Import integ_rel.sh <wt> <log> 'TestOldImportsTaskFilesAreListedByTheirTasks'

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestOldImportsTaskFilesAreListedByTheirTasks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "jira", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusCompleted}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	ada, hr1, gone := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ada@relink.test', 'ada', 'Ada', NOW(), NOW())`, ada)
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, NULL, $2)`, hr1, ada)

	// The files as imports before SF1 left them: rows and records for each;
	// HR-1's letters in the graph unlisted, made by a stray node holding Ada's
	// uuid, which her next connect typed a User; HR-1's table not in the graph
	// at all; and HR-1's brief, kept since F, as two nodes, the second listed.
	letters, table, brief, lost, removed, onComment, notKept := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live},
		{"uid": "_:hr1", "dgraph.type": "Task", "task_uuid": hr1.String(), "task_name": "HR-1", "task_deleted_at": live,
			"task_attachments": []map[string]any{{"uid": "_:brief2"}}},
		{"uid": "_:brief1", "dgraph.type": "Attachment", "attachment_uuid": brief.String(), "attachment_file_name": "brief.md"},
		{"uid": "_:brief2", "dgraph.type": "Attachment", "attachment_uuid": brief.String(), "attachment_file_name": "brief.md"},
	})
	// The stray comes after Ada's own node, as an import's always did.
	dg.Mutate(t, []map[string]any{
		{"uid": "_:letters", "dgraph.type": "Attachment", "attachment_uuid": letters.String(), "attachment_file_name": "offer-letters.pdf",
			"attachment_obj_key": ada.String() + "/offer-letters.pdf", "attachment_created_by": map[string]any{"uid": "_:stray"}},
		{"uid": "_:stray", "dgraph.type": "User", "user_uuid": ada.String(), "devices_connected": 1},
	})
	files := []struct {
		id          uuid.UUID
		name, kind  string
		holder      uuid.UUID
		row         bool
		deleted     bool
		placeholder bool
	}{
		{letters, "offer-letters.pdf", "task", hr1, true, false, false},
		{table, "raise-table.xlsx", "task", hr1, true, false, false},
		{brief, "brief.md", "task", hr1, true, false, false},
		{lost, "old-plan.txt", "task", gone, true, false, false},
		{removed, "draft.txt", "task", hr1, true, true, false},
		{onComment, "notes.txt", "comment", uuid.New(), true, false, false},
		{notKept, "huge.mov", "task", hr1, false, false, true},
	}
	for i, f := range files {
		if f.row {
			exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by, deleted_at)
				VALUES ($1, $2, 'task', $3, $4, CASE WHEN $5 THEN NOW() END)`, f.id, ada.String()+"/"+f.name, f.holder.String(), ada, f.deleted)
		}
		rec := fileRecord{Name: f.name, ObjKey: ada.String() + "/" + f.name, Size: int64(100 + i), ContentType: "application/octet-stream",
			ParentKind: f.kind, ParentUUID: f.holder.String(), Placeholder: f.placeholder}
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityFile, "f-"+f.name, f.id, nil, mustMarshal(rec), true); err != nil {
			t.Fatal(err)
		}
	}

	// The graph refuses the table's first link: the pass goes on past it, and
	// isn't recorded as done, so the next one links it.
	was, refused := linkFile, false
	linkFile = func(ctx context.Context, kind, holder string, file *dgraphStruct.DgraphAttachment) error {
		if file.Uuid == table.String() && !refused {
			refused = true
			return errors.New("the graph is away")
		}
		return was(ctx, kind, holder, file)
	}
	t.Cleanup(func() { linkFile = was })
	rep, ran, err := TaskFileRepair.Run(ctx)
	if err == nil || !ran || rep != (taskFileRepair{Linked: 1, Listed: 1, NoTask: 1, Failed: 1}) {
		t.Fatalf("the first pass: %+v ran %v (%v); want the letters linked, the brief left, the lost task's file counted and the table's failure answered", rep, ran, err)
	}
	rep, ran, err = TaskFileRepair.Run(ctx)
	if err != nil || !ran || rep != (taskFileRepair{Linked: 1, Listed: 2, NoTask: 1}) {
		t.Fatalf("the second pass: %+v ran %v (%v); want the table linked, the letters and the brief left", rep, ran, err)
	}
	if _, ran, err := TaskFileRepair.Run(ctx); ran || err != nil {
		t.Errorf("a third pass ran (%v, %v); want the repair recorded as done", ran, err)
	}

	// HR-1 lists its three kept files once each, and not the removed one; the
	// letters and the table are one node each, made by Ada's own node.
	graph, err := dgexec.Read[struct {
		Task []struct {
			Files []struct {
				Uuid string `json:"attachment_uuid"`
			} `json:"task_attachments"`
		} `json:"task"`
		Letters, Table []keptNode
	}](ctx, `query Listed($task: string, $letters: string, $table: string) {
		task(func: eq(task_uuid, $task)) { task_attachments { attachment_uuid } }
		letters(func: eq(attachment_uuid, $letters)) { attachment_uuid attachment_file_name attachment_created_by { uid } }
		table(func: eq(attachment_uuid, $table)) { attachment_uuid attachment_file_name attachment_created_by { uid } }
	}`, map[string]string{"$task": hr1.String(), "$letters": letters.String(), "$table": table.String()})
	if err != nil || len(graph.Task) != 1 {
		t.Fatalf("HR-1 in the graph: %+v (%v)", graph, err)
	}
	var listed []string
	for _, f := range graph.Task[0].Files {
		listed = append(listed, f.Uuid)
	}
	slices.Sort(listed)
	want := []string{letters.String(), table.String(), brief.String()}
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("HR-1 lists %v; want its letters, table and brief once each (%v)", listed, want)
	}
	for name, nodes := range map[string][]keptNode{"offer-letters.pdf": graph.Letters, "raise-table.xlsx": graph.Table} {
		if len(nodes) != 1 || nodes[0].Name != name || nodes[0].By.Uid != uids["ada"] {
			t.Errorf("%s in the graph: %+v; want one node, made by Ada's own node (%s)", name, nodes, uids["ada"])
		}
	}
}

// keptNode is what the repair test reads of a kept file's node.
type keptNode struct {
	Name string `json:"attachment_file_name"`
	By   struct {
		Uid string `json:"uid"`
	} `json:"attachment_created_by"`
}
