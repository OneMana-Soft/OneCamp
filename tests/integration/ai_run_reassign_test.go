//go:build integration

package integration_test

// CR-352's own attack test for V-1096 (review only; not part of the head).
// An agent's run searches for its sponsor AND the person who asked; the
// asker is a task's former assignee who left its project and was invited to
// a DIFFERENT project. Neither the hand-over nor a stale entry may give them
// the task through the run, while its current assignee and the project's own
// people still find it.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCR352ARunForAFormerAssigneeInAnotherProjectLeavesTheTaskOut(t *testing.T) {
	ctx := context.Background()
	env := integration.ConnectServerPostgres(t)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	integration.SetupOpenSearch(t)

	model := httptest.NewServer(embedOnlyModel{})
	defer model.Close()
	t.Setenv("OLLAMA_HOST", model.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000`, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil {
		t.Fatalf("init AI: %v", err)
	}
	if !ai.GetService().IsEnabled() {
		t.Fatal("AI did not come up enabled")
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	launch, side, team := uuid.New(), uuid.New(), uuid.New()
	for _, key := range []string{"olu", "mia", "xan", "yuri"} {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@cr352.test', $2 || '352', $2, NOW(), NOW())`, id, key)
		node := map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": key + "352", "user_deleted_at": live}
		switch key {
		case "olu", "mia":
			node["user_projects"] = []map[string]any{{"uid": "_:launch"}}
		case "xan":
			// Left the launch; invited to a different project since.
			node["user_projects"] = []map[string]any{{"uid": "_:side"}}
		}
		nodes = append(nodes, node)
	}
	roadmap, minutes := uuid.New(), uuid.New()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch352', $3), ($2, 'Side352', $3)`, launch, side, people["olu"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, roadmap, minutes, launch, people["olu"])
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Core352",
			"team_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:mia"}, {"uid": "_:xan"}}, "team_admins": []map[string]any{{"uid": "_:olu"}}},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch352",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"},
			"project_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:mia"}}, "project_admins": []map[string]any{{"uid": "_:olu"}},
			"project_created_by": map[string]any{"uid": "_:olu"}, "project_tasks": []map[string]any{{"uid": "_:roadmap"}, {"uid": "_:minutes"}}},
		map[string]any{"uid": "_:side", "dgraph.type": "Project", "project_uuid": side.String(), "project_name": "Side352",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"},
			"project_members": []map[string]any{{"uid": "_:xan"}, {"uid": "_:olu"}}, "project_admins": []map[string]any{{"uid": "_:olu"}},
			"project_created_by": map[string]any{"uid": "_:olu"}},
		map[string]any{"uid": "_:roadmap", "dgraph.type": "Task", "task_uuid": roadmap.String(), "task_name": "quokka-roadmap", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:xan"}},
		// Went from Xan to Yuri before the fix: the entry still names Xan.
		map[string]any{"uid": "_:minutes", "dgraph.type": "Task", "task_uuid": minutes.String(), "task_name": "quokka-minutes", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:yuri"}},
	)
	dg.Mutate(t, nodes)
	ai.EmbedTaskContent("quokka-roadmap", "", roadmap.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
	ai.EmbedTaskContent("quokka-minutes", "", minutes.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")

	user := func(key string) *dgraphStruct.DgraphUser {
		t.Helper()
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[key].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s: %v", key, err)
		}
		return gr
	}
	olu := user("olu")
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["olu"])
	if err != nil || pg == nil {
		t.Fatalf("load olu: %v", err)
	}
	asOlu := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *olu})

	// run is where a run Olu sponsors, asked for by asker, finds the marked
	// task, searching as Olu in the launch.
	launchOnly := []string{launch.String()}
	where := func(c context.Context, reader string, projects []string, marker string, id uuid.UUID) []string {
		t.Helper()
		var got []string
		hit := func(cu, text string) bool {
			return strings.EqualFold(cu, id.String()) || strings.Contains(text, marker)
		}
		sim, err := ai.SearchSimilar(c, marker, people[reader].String(), nil, projects, nil, 50)
		if err != nil {
			t.Fatalf("SearchSimilar: %v", err)
		}
		for _, h := range sim {
			if hit(h.ContentUUID, h.ContentText) {
				got = append(got, "SearchSimilar")
				break
			}
		}
		rec, err := ai.SearchRecentGlobal(c, people[reader].String(), nil, projects, nil, 50)
		if err != nil {
			t.Fatalf("SearchRecentGlobal: %v", err)
		}
		for _, h := range rec {
			if hit(h.ContentUUID, h.ContentText) {
				got = append(got, "SearchRecentGlobal")
				break
			}
		}
		un, err := ai.FetchUnreadAcrossScopes(c, people[reader].String(), nil, projects, nil, 0, 50)
		if err != nil {
			t.Fatalf("FetchUnreadAcrossScopes: %v", err)
		}
		for _, h := range un {
			if hit(h.ContentUUID, h.ContentText) {
				got = append(got, "FetchUnreadAcrossScopes")
				break
			}
		}
		return got
	}
	run := func(asker, marker string, id uuid.UUID) []string {
		t.Helper()
		return where(ai.WithRunRequester(ctx, people[asker].String(), people["olu"].String()), "olu", launchOnly, marker, id)
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("%s: not after 20s", what)
	}

	// Controls: a run Mia (one of the launch's people) asks for finds both;
	// a run Xan asks for finds the roadmap while it is still his.
	eventually("a run Mia asks for finds both", func() bool {
		return len(run("mia", "quokka-roadmap", roadmap)) == 3 && len(run("mia", "quokka-minutes", minutes)) == 3
	})
	eventually("a run Xan asks for finds the roadmap while it is his", func() bool { return len(run("xan", "quokka-roadmap", roadmap)) == 3 })

	info, err := taskBusiness.GetDgraphBasicTaskInfo(asOlu, roadmap.String(), olu.Uid)
	if err != nil || info == nil {
		t.Fatalf("read roadmap: %v", err)
	}
	if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(asOlu), roadmap, user("yuri"), user("xan").Uid, info.Uid, info, olu); err != nil {
		t.Fatalf("hand the roadmap to Yuri: %v", err)
	}

	t.Run("a run the former assignee asks for, sponsored by one of the project's people, leaves the handed-over task out", func(t *testing.T) {
		if got := run("xan", "quokka-roadmap", roadmap); len(got) != 0 {
			t.Errorf("a run Xan asked for still reads the roadmap through %v", got)
		}
	})
	t.Run("a run the former assignee asks for leaves a stale entry's task out", func(t *testing.T) {
		if got := run("xan", "quokka-minutes", minutes); len(got) != 0 {
			t.Errorf("a run Xan asked for still reads the minutes through %v", got)
		}
	})
	t.Run("the former assignee in a different project reads neither himself", func(t *testing.T) {
		sideOnly := []string{side.String()}
		if got := where(ctx, "xan", sideOnly, "quokka-roadmap", roadmap); len(got) != 0 {
			t.Errorf("Xan (in Side) still reads the roadmap through %v", got)
		}
		if got := where(ctx, "xan", sideOnly, "quokka-minutes", minutes); len(got) != 0 {
			t.Errorf("Xan (in Side) still reads the minutes through %v", got)
		}
	})
	t.Run("a run the new assignee asks for finds the handed-over task; one of the project's people still finds both", func(t *testing.T) {
		if got := run("yuri", "quokka-roadmap", roadmap); len(got) != 3 {
			t.Errorf("a run Yuri asked for finds the roadmap only through %v", got)
		}
		if got := run("mia", "quokka-minutes", minutes); len(got) != 3 {
			t.Errorf("a run Mia asked for finds the minutes only through %v", got)
		}
	})
}
