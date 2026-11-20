//go:build integration

package integration_test

// Ask AI reads a task as the app does: its project's people, its assignee,
// and the maker of a task in no project. The AI index copied the assignee
// when the task was made and nothing wrote it again, so the search granted
// the task to whoever it was first given to. Xan, outside the project, was
// given the press kit by an import and kept reading it through Ask AI after
// it went to Yuri; Mia, one of the project's people and the assignee of the
// budget, kept reading it after she left the project and was taken off it.
// Each of the three searches Ask AI makes now leaves those tasks out, and
// Yuri, the press kit's new assignee from outside the project, finds it.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestAskAIFollowsATasksCurrentAssignee$' -v

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAskAIFollowsATasksCurrentAssignee(t *testing.T) {
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
		t.Fatal("AI did not come up enabled; nothing below would run")
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
	for _, key := range []string{"olu", "xan", "yuri", "mia"} {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@reassign.test', $2, $2, NOW(), NOW())`, id, key)
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": key, "user_deleted_at": live})
	}
	// Olu runs the launch with Mia; Xan and Yuri are outside it. The press kit
	// came in with an import as Xan's; the budget is Mia's.
	// The poster went from Xan to Yuri before this was fixed, so its entry
	// still names Xan; the sketch is Xan's own, in no project.
	launch, team, kit, budget, note := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	poster, sketch := uuid.New(), uuid.New()
	// The vault is restricted: Olu, who runs the launch, keeps it to himself,
	// so Mia, one of its people, may not read it.
	vault := uuid.New()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, launch, people["olu"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4), ($5, $3, $4)`, kit, budget, launch, people["olu"], poster)
	exec(`INSERT INTO tasks (id, created_by) VALUES ($1, $2)`, sketch, people["xan"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, vault, launch, people["olu"])
	exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, vault, launch, people["olu"])
	exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, note, people["olu"])
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Core",
			"team_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:mia"}}, "team_admins": []map[string]any{{"uid": "_:olu"}}},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"},
			"project_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:mia"}}, "project_admins": []map[string]any{{"uid": "_:olu"}},
			"project_created_by": map[string]any{"uid": "_:olu"}, "project_tasks": []map[string]any{{"uid": "_:kit"}, {"uid": "_:budget"}, {"uid": "_:vault"}}},
		// A project's people have it on their own side too (user_projects), as
		// adding a member writes it: what a person can see when an agent's run
		// is for them is read from there (business/AI readerScopeFor).
		map[string]any{"uid": "_:olu", "user_projects": []map[string]any{{"uid": "_:launch"}}},
		map[string]any{"uid": "_:mia", "user_projects": []map[string]any{{"uid": "_:launch"}}},
		map[string]any{"uid": "_:kit", "dgraph.type": "Task", "task_uuid": kit.String(), "task_name": "zebra-alpha press kit", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:xan"}},
		map[string]any{"uid": "_:budget", "dgraph.type": "Task", "task_uuid": budget.String(), "task_name": "zebra-bravo budget", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:mia"}},
	)
	nodes = append(nodes,
		map[string]any{"uid": "_:poster", "dgraph.type": "Task", "task_uuid": poster.String(), "task_name": "zebra-charlie poster", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:yuri"}},
		map[string]any{"uid": "_:vault", "dgraph.type": "Task", "task_uuid": vault.String(), "task_name": "zebra-echo vault", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"}},
		map[string]any{"uid": "_:sketch", "dgraph.type": "Task", "task_uuid": sketch.String(), "task_name": "zebra-delta sketch", "task_status": "todo",
			"task_deleted_at": live, "task_created_by": map[string]any{"uid": "_:xan"}},
	)
	uids := dg.Mutate(t, nodes)

	// The entries as making the tasks writes them (business/Task CreateTask,
	// createMany, an import's finish), and a comment on the budget.
	ai.EmbedTaskContent("zebra-alpha press kit", "", kit.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
	ai.EmbedTaskContent("zebra-bravo budget", "", budget.String(), launch.String(), team.String(), people["mia"].String(), people["olu"].String(), "olu")
	ai.EmbedTaskContent("zebra-charlie poster", "", poster.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
	ai.EmbedTaskContent("zebra-echo vault", "", vault.String(), launch.String(), team.String(), "", people["olu"].String(), "olu")
	ai.EmbedTaskContent("zebra-delta sketch", "", sketch.String(), "", "", "", people["xan"].String(), "xan")
	ai.EmbedTaskCommentContent("zebra-bravo numbers are in", note.String(), people["olu"].String(), "olu", launch.String(), budget.String(), team.String())

	user := func(key string) *dgraphStruct.DgraphUser {
		t.Helper()
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[key].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", key, err)
		}
		return gr
	}
	olu := user("olu")
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["olu"])
	if err != nil || pg == nil {
		t.Fatalf("load olu: %v", err)
	}
	asOlu := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *olu})
	hand := func(c context.Context, task uuid.UUID, from, to *dgraphStruct.DgraphUser) {
		t.Helper()
		info, err := taskBusiness.GetDgraphBasicTaskInfo(asOlu, task.String(), olu.Uid)
		if err != nil || info == nil {
			t.Fatalf("read task %s: %v", task, err)
		}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(c, task, to, from.Uid, info.Uid, info, olu); err != nil {
			t.Fatalf("hand task %s over: %v", task, err)
		}
	}

	// found names which of the three searches Ask AI makes return the marked
	// text to the reader, who is in the projects named.
	// c is the ask's context (an agent's run for someone carries them).
	foundIn := func(c context.Context, reader string, projects []string, marker string, ids ...uuid.UUID) []string {
		t.Helper()
		match := func(id, text string) bool {
			for _, want := range ids {
				if strings.EqualFold(id, want.String()) {
					return true
				}
			}
			return strings.Contains(text, marker)
		}
		var where []string
		hits, err := ai.SearchSimilar(c, marker, people[reader].String(), nil, projects, nil, 50)
		if err != nil {
			t.Fatalf("SearchSimilar: %v", err)
		}
		for _, h := range hits {
			if match(h.ContentUUID, h.ContentText) {
				where = append(where, "SearchSimilar")
				break
			}
		}
		recent, err := ai.SearchRecentGlobal(c, people[reader].String(), nil, projects, nil, 50)
		if err != nil {
			t.Fatalf("SearchRecentGlobal: %v", err)
		}
		for _, h := range recent {
			if match(h.ContentUUID, h.ContentText) {
				where = append(where, "SearchRecentGlobal")
				break
			}
		}
		unread, err := ai.FetchUnreadAcrossScopes(c, people[reader].String(), nil, projects, nil, 0, 50)
		if err != nil {
			t.Fatalf("FetchUnreadAcrossScopes: %v", err)
		}
		for _, h := range unread {
			if match(h.ContentUUID, h.ContentText) {
				where = append(where, "FetchUnreadAcrossScopes")
				break
			}
		}
		return where
	}
	found := func(reader string, projects []string, marker string, ids ...uuid.UUID) []string {
		t.Helper()
		return foundIn(ctx, reader, projects, marker, ids...)
	}
	all := []string{"SearchSimilar", "SearchRecentGlobal", "FetchUnreadAcrossScopes"}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("%s: not after 20s", what)
	}
	launchOnly := []string{launch.String()}

	// The controls: the entries are written, and each assignee finds their
	// task in all three searches.
	eventually("Xan finds the press kit he was given", func() bool { return len(found("xan", nil, "zebra-alpha", kit)) == 3 })
	eventually("Mia finds the budget and its comment", func() bool {
		return len(found("mia", launchOnly, "zebra-bravo", budget)) == 3 && len(found("mia", launchOnly, "numbers are in", note)) == 3
	})
	eventually("Olu finds the poster", func() bool { return len(found("olu", launchOnly, "zebra-charlie", poster)) == 3 })
	eventually("Olu finds the vault he keeps", func() bool { return len(found("olu", launchOnly, "zebra-echo", vault)) == 3 })
	eventually("Xan finds the sketch he made", func() bool { return len(found("xan", nil, "zebra-delta", sketch)) == 3 })

	t.Run("D: an entry left naming an old assignee gives them nothing", func(t *testing.T) {
		if got := found("xan", nil, "zebra-charlie", poster); len(got) != 0 {
			t.Errorf("Xan, whose poster went to Yuri before the fix, still reads it through %v", got)
		}
	})

	t.Run("E: the maker of a task in no project still reads it", func(t *testing.T) {
		if got := found("xan", nil, "zebra-delta", sketch); len(got) != len(all) {
			t.Errorf("Xan finds his own sketch only through %v of %v", got, all)
		}
	})

	t.Run("A: an outsider handed a task, then not, no longer reads it through Ask AI", func(t *testing.T) {
		hand(taskBusiness.DataMove(asOlu), kit, user("xan"), user("yuri"))
		if got := found("xan", nil, "zebra-alpha", kit); len(got) != 0 {
			t.Errorf("Xan, no longer the press kit's assignee and outside its project, still reads it through %v", got)
		}
	})

	t.Run("C: the new assignee from outside the project finds it", func(t *testing.T) {
		if got := found("yuri", nil, "zebra-alpha", kit); len(got) != len(all) {
			t.Errorf("Yuri, the press kit's assignee now, finds it only through %v of %v", got, all)
		}
	})

	t.Run("F: an agent's run for an old assignee outside the project gives them nothing", func(t *testing.T) {
		// Olu's agent, asked by Xan: Olu's projects cover both tasks, so only
		// Xan's half of the check can keep them from him.
		asXan := ai.WithRunRequester(ctx, people["xan"].String(), people["olu"].String())
		if got := foundIn(asXan, "olu", launchOnly, "zebra-charlie", poster); len(got) != 0 {
			t.Errorf("Xan, asking Olu's agent, reads the poster whose entry still names him through %v", got)
		}
		if got := foundIn(asXan, "olu", launchOnly, "zebra-alpha", kit); len(got) != 0 {
			t.Errorf("Xan, asking Olu's agent, reads the press kit he was taken off through %v", got)
		}
		asYuri := ai.WithRunRequester(ctx, people["yuri"].String(), people["olu"].String())
		if got := foundIn(asYuri, "olu", launchOnly, "zebra-alpha", kit); len(got) != len(all) {
			t.Errorf("Yuri, the press kit's assignee, asking Olu's agent finds it only through %v of %v", got, all)
		}
	})

	t.Run("G: an agent's run keeps a task restricted from the person who asked from them", func(t *testing.T) {
		if got := found("mia", launchOnly, "zebra-echo", vault); len(got) != 0 {
			t.Fatalf("Mia, asking herself, reads the vault kept from her through %v", got)
		}
		// Olu's agent, asked by Mia: the vault is Olu's to read, not hers.
		asMia := ai.WithRunRequester(ctx, people["mia"].String(), people["olu"].String())
		if got := foundIn(asMia, "olu", launchOnly, "zebra-echo", vault); len(got) != 0 {
			t.Errorf("Mia, asking Olu's agent, reads the vault kept from her through %v", got)
		}
		if got := found("olu", launchOnly, "zebra-echo", vault); len(got) != len(all) {
			t.Errorf("Olu finds his vault only through %v of %v", got, all)
		}
	})

	t.Run("B: one of the project's people who left it and was taken off the task no longer reads it", func(t *testing.T) {
		// Both edges, as RemoveMemberFromProject takes them.
		drop := []byte(`[{"uid": "` + uids["launch"] + `", "project_members": [{"uid": "` + uids["mia"] + `"}]}, {"uid": "` + uids["mia"] + `", "user_projects": [{"uid": "` + uids["launch"] + `"}]}]`)
		if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{DeleteJson: drop}}}); err != nil {
			t.Fatalf("take Mia out of the launch: %v", err)
		}
		hand(asOlu, budget, user("mia"), nil)
		if got := found("mia", nil, "zebra-bravo", budget); len(got) != 0 {
			t.Errorf("Mia, out of the project and off the budget, still reads it through %v", got)
		}
		if got := found("mia", nil, "numbers are in", note); len(got) != 0 {
			t.Errorf("Mia, out of the project, still reads a comment on the budget through %v", got)
		}
		// Olu, one of its people, still reads both.
		if got := found("olu", launchOnly, "zebra-bravo", budget); len(got) != len(all) {
			t.Errorf("Olu, in the project, finds the budget only through %v", got)
		}
	})
}
