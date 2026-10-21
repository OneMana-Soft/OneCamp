//go:build integration

package business

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A task lists only files its writer uploaded to its project. Member B
// naming member A's file (uploaded to A's private channel), a file B uploaded
// somewhere else, or an id nobody uploaded, wrote an entry onto B's task that
// opened for no one: a file is served through its place, and none of those is
// the project's. B's own upload to the project is kept, with the storage key
// its upload stored, whatever key the request named.
func TestATaskListsOnlyItsWritersOwnUploads(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	// The task's files go to search too (a goroutine of their own).
	integration.SetupOpenSearch(t)

	a, b, project, task, channel := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{a, b} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, u.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name) VALUES ($1, 'launch')`, project); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, b); err != nil {
		t.Fatal(err)
	}
	file := func(by uuid.UUID, srcKey, srcValue, key string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`, id, key, srcKey, srcValue, by); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mine := file(b, "project", project.String(), b.String()+"/plan.png")
	theirs := file(a, "channel", channel.String(), a.String()+"/salaries.xlsx")
	elsewhere := file(b, "channel", channel.String(), b.String()+"/elsewhere.png")
	never := uuid.New()

	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "launch"},
		{"uid": "_:t", "dgraph.type": "Task", "task_uuid": task.String(), "task_name": "Plan", "task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]string{"uid": "_:p"}},
		{"uid": "_:b", "dgraph.type": "User", "user_uuid": b.String(), "user_name": "bee"},
	})
	writer := &dgraphStruct.DgraphUser{Uid: uids["b"], Uuid: b.String()}
	info, err := GetDgraphBasicTaskInfo(ctx, task.String(), writer.Uid)
	if err != nil || info == nil || info.Project == nil {
		t.Fatalf("the task: %+v %v", info, err)
	}
	att := func(id uuid.UUID, key string) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: id.String(), ObjectKey: key, FileName: key}
	}
	if err := AddAttachmentToTask(ctx, task, info, &adapter.CreateOrUpdateTaskInput{Attachments: []*dgraphStruct.DgraphAttachment{
		att(mine, "someone-else/forged-key.png"),
		att(theirs, a.String()+"/salaries.xlsx"),
		att(elsewhere, b.String()+"/elsewhere.png"),
		att(never, "made/up.png"),
	}}, writer); err != nil {
		t.Fatal(err)
	}

	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
		`query q($id: string) { t(func: eq(task_uuid, $id)) { task_attachments { attachment_uuid attachment_obj_key } } }`,
		map[string]string{"$id": task.String()})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		T []struct {
			Files []struct {
				ID  string `json:"attachment_uuid"`
				Key string `json:"attachment_obj_key"`
			} `json:"task_attachments"`
		}
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.T) != 1 {
		t.Fatalf("read back: %v %s", err, resp.Json)
	}
	var got []string
	for _, f := range out.T[0].Files {
		got = append(got, f.ID+" "+f.Key)
	}
	if want := []string{mine.String() + " " + b.String() + "/plan.png"}; !slices.Equal(got, want) {
		t.Errorf("the task lists %v; want only B's own upload to the project, with its stored key: %v", got, want)
	}
}
