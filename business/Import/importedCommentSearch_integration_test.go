//go:build integration

package business

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// On this line an import writes a task comment's search entry with its task
// and no project (writeTaskComment), and search lets a comment in only
// through a place it names: a channel, a chat, a project, a doc or a board.
// So nobody finds an imported comment in search, not even the project's own
// members, and an old import's comments need no holding back until their
// import is re-checked (beta's comment search repair, which beta needed once
// its imported comments' entries named their projects).
func TestAnImportedCommentIsNotFoundInSearch(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)
	env, dg := world.env, world.dg

	// Ada imports; Ben is a member of the project, and the comment's author.
	people := map[string]uuid.UUID{"ada": uuid.New(), "ben": uuid.New()}
	for n, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, n); err != nil {
			t.Fatal(err)
		}
	}
	project, task := uuid.New(), uuid.New()
	live := "0001-01-01T00:00:00Z"
	members := []map[string]any{{"uid": "_:ada"}, {"uid": "_:ben"}}
	nodes := []map[string]any{
		{"uid": "_:hr", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "HR", "project_deleted_at": live,
			"project_members": members, "project_admins": []map[string]any{{"uid": "_:ada"}}},
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": task.String(), "task_name": "Desk move", "task_deleted_at": live,
			"task_project": map[string]any{"uid": "_:hr"}},
	}
	for n, id := range people {
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": id.String(), "user_name": n,
			"user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:hr"}}})
	}
	uids := dg.Mutate(t, nodes)

	// The import's own write of a comment, on the task as its comment worker
	// reads it.
	work := importContext(ctx)
	dgTask, err := taskDomain.GetDgraphBasicTaskInfoByUUID(work, task.String(), uids["ada"])
	if err != nil || dgTask == nil || dgTask.Project == nil {
		t.Fatalf("the task as the import reads it: %+v (%v)", dgTask, err)
	}
	ben := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: people["ben"]}}
	ben.UserDgraphInfo.Uid, ben.UserDgraphInfo.Uuid = uids["ben"], people["ben"].String()
	comment, err := writeTaskComment(work, dgTask, ben, "Wombat budget signed off", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// Its entry is written on its own goroutine: wait until search holds it.
	written := func() bool {
		resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: openSearchStruct.COMMENT_INDEX, DocumentID: comment.String()})
		if err != nil {
			return false
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Found bool `json:"found"`
		}
		return json.Unmarshal(raw, &got) == nil && got.Found
	}
	for deadline := time.Now().Add(30 * time.Second); !written(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the imported comment's search entry was never written")
		}
	}
	if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.COMMENT_INDEX); err != nil {
		t.Fatal(err)
	}

	for _, who := range []string{"ada", "ben"} {
		if out, n := searchAs(t, ctx, people[who], "wombat"); n != 0 {
			t.Errorf("%s, a member of the comment's project, finds %d for its words; want nothing: %s", who, n, out)
		}
	}
}
