//go:build integration

package integration_test

// A task's AI entry names its assignee, and Ask AI finds the task for them
// from outside its project through that alone. Hand-overs are written onto
// the entry now, but an entry can still name someone the task has gone from:
// a hand-over made before they were written, or one whose write failed, or
// one made while the task's entry was still being made (the entry is embedded
// first and written seconds later, after the hand-over's write found nothing).
// The task's assignee now, from outside the project, then never finds it.
//
// H: the startup repair writes each task's assignee now onto its entry, and
// counts the one it corrected; I: it is recorded done, and finds nothing more
// to correct; J: an entry being made while its task is handed over is written
// with the assignee it has when it is written.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestAskAIEntriesCatchUpWithTheirTasksAssignee$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// heldModel is embedOnlyModel that, once armed, holds the next embedding it
// is asked for until it is let go: the seconds a model takes.
type heldModel struct {
	armed atomic.Bool
	held  chan struct{}
	letGo chan struct{}
}

func (m *heldModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/embed" && m.armed.CompareAndSwap(true, false) {
		close(m.held)
		select {
		case <-m.letGo:
		case <-time.After(12 * time.Second):
		}
	}
	embedOnlyModel{}.ServeHTTP(w, r)
}

func TestAskAIEntriesCatchUpWithTheirTasksAssignee(t *testing.T) {
	ctx := context.Background()
	env := integration.ConnectServerPostgres(t)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)

	model := &heldModel{held: make(chan struct{}), letGo: make(chan struct{})}
	srv := httptest.NewServer(model)
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
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
	for _, key := range []string{"olu", "xan", "yuri"} {
		id := uuid.New()
		people[key] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@backfill.test', $2, $2, NOW(), NOW())`, id, key+"-bf")
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "User", "user_uuid": id.String(), "user_name": key, "user_deleted_at": live})
	}
	// Olu runs the launch; Xan and Yuri are outside it. The poster, the flyer
	// and the banner are Xan's to begin with; the banner is in the trash.
	launch, team, poster, flyer, banner := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, launch, people["olu"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4), ($5, $3, $4)`, poster, flyer, launch, people["olu"], banner)
	task := func(key string, id uuid.UUID, name string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": name, "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": map[string]any{"uid": "_:olu"},
			"task_assignee": map[string]any{"uid": "_:xan"}}
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:core", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Core",
			"team_members": []map[string]any{{"uid": "_:olu"}}, "team_admins": []map[string]any{{"uid": "_:olu"}}},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_team": map[string]any{"uid": "_:core"},
			"project_members": []map[string]any{{"uid": "_:olu"}}, "project_admins": []map[string]any{{"uid": "_:olu"}},
			"project_created_by": map[string]any{"uid": "_:olu"}, "project_tasks": []map[string]any{{"uid": "_:poster"}, {"uid": "_:flyer"}, {"uid": "_:banner"}}},
		map[string]any{"uid": "_:olu", "user_projects": []map[string]any{{"uid": "_:launch"}}},
		task("poster", poster, "yankee-alpha poster"),
		task("flyer", flyer, "yankee-bravo flyer"),
	)
	trashed := task("banner", banner, "yankee-charlie banner")
	trashed["task_deleted_at"] = "2026-11-01T00:00:00Z"
	nodes = append(nodes, trashed)
	uids := dg.Mutate(t, nodes)

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

	// finds is whether the reader, outside the launch, finds the marked task
	// through Ask AI's similar search.
	finds := func(reader, marker string, id uuid.UUID) bool {
		t.Helper()
		hits, err := ai.SearchSimilar(ctx, marker, people[reader].String(), nil, nil, nil, 50)
		if err != nil {
			t.Fatalf("SearchSimilar: %v", err)
		}
		for _, h := range hits {
			if strings.EqualFold(h.ContentUUID, id.String()) {
				return true
			}
		}
		return false
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

	// named is whom the task's AI entry names, read from the index itself:
	// every search leaves a trashed task out.
	named := func(id uuid.UUID) string {
		t.Helper()
		resp, err := http.Get(search.URL + "/" + ai.AI_EMBEDDINGS_INDEX + "/_doc/task:" + id.String())
		if err != nil {
			t.Fatalf("read task %s's entry: %v", id, err)
		}
		defer resp.Body.Close()
		var entry struct {
			Source struct {
				Assignee string `json:"task_assignee_user_id"`
			} `json:"_source"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
			t.Fatalf("read task %s's entry: %v", id, err)
		}
		return entry.Source.Assignee
	}

	// The poster's entry and the banner's, written while they were Xan's.
	ai.EmbedTaskContent("yankee-alpha poster", "", poster.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
	ai.EmbedTaskContent("yankee-charlie banner", "", banner.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
	eventually("Xan finds the poster he was given", func() bool { return finds("xan", "yankee-alpha", poster) })
	eventually("the banner's entry names Xan", func() bool { return named(banner) == people["xan"].String() })
	// Before hand-overs were written onto entries: the poster and the banner
	// went to Yuri in the graph alone, so their entries still name Xan.
	for _, key := range []string{"poster", "banner"} {
		move := []byte(`{"uid": "` + uids[key] + `", "task_assignee": {"uid": "` + uids["yuri"] + `"}}`)
		drop := []byte(`{"uid": "` + uids[key] + `", "task_assignee": {"uid": "` + uids["xan"] + `"}}`)
		if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{DeleteJson: drop}, {SetJson: move}}}); err != nil {
			t.Fatalf("give the %s to Yuri in the graph alone: %v", key, err)
		}
	}
	if finds("yuri", "yankee-alpha", poster) {
		t.Fatal("the world before the repair isn't as built: Yuri already finds the poster")
	}

	repair := taskBusiness.AIAssigneeBackfill
	if repair.Key != "ai_embeddings_task_assignee_v1" {
		t.Fatalf("the repair's key is %q", repair.Key)
	}
	exec(`DELETE FROM system_configs WHERE key = $1`, repair.Key)

	t.Run("H: the repair writes each task's assignee now onto its entry", func(t *testing.T) {
		rep, ran, err := repair.Run(ctx)
		if err != nil || !ran {
			t.Fatalf("ran %v: %+v %v", ran, rep, err)
		}
		if rep.Corrected != 2 || rep.Tasks < 3 {
			t.Errorf("the repair read %d tasks and corrected %d entries, want 2 corrected (the poster's, the banner's) of at least 3", rep.Tasks, rep.Corrected)
		}
		if got := named(banner); got != people["yuri"].String() {
			t.Errorf("the trashed banner's entry names %q after the repair, not Yuri, so a restore brings it back naming Xan", got)
		}
		if !finds("yuri", "yankee-alpha", poster) {
			t.Error("Yuri, the poster's assignee from outside the project, doesn't find it after the repair")
		}
		if finds("xan", "yankee-alpha", poster) {
			t.Error("Xan, whom the poster has gone from, finds it after the repair")
		}
	})

	t.Run("I: the repair is recorded done, and finds nothing more to correct", func(t *testing.T) {
		var record string
		if err := env.PG.QueryRow(`SELECT value FROM system_configs WHERE key = $1`, repair.Key).Scan(&record); err != nil || !strings.HasPrefix(record, "done: ") {
			t.Fatalf("the repair's record: %q %v", record, err)
		}
		if !strings.Contains(record, "corrected 2 entries") {
			t.Errorf("the repair's record doesn't say it corrected the poster's and the banner's entries: %q", record)
		}
		if _, ran, err := repair.Run(ctx); ran || err != nil {
			t.Errorf("a start after it was recorded ran it again: ran %v %v", ran, err)
		}
		rep, err := repair.Repair(ctx)
		if err != nil || rep.Corrected != 0 || rep.Tasks < 3 {
			t.Errorf("a second pass: %+v %v, want at least 3 tasks read and 0 corrected", rep, err)
		}
	})

	t.Run("J: a hand-over while the entry is being made is on the entry it makes", func(t *testing.T) {
		model.armed.Store(true)
		ai.EmbedTaskContent("yankee-bravo flyer", "", flyer.String(), launch.String(), team.String(), people["xan"].String(), people["olu"].String(), "olu")
		select {
		case <-model.held:
		case <-time.After(10 * time.Second):
			t.Fatal("the flyer's entry never asked for its embedding")
		}
		info, err := taskBusiness.GetDgraphBasicTaskInfo(asOlu, flyer.String(), olu.Uid)
		if err != nil || info == nil {
			t.Fatalf("read the flyer: %v", err)
		}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(asOlu), flyer, user("yuri"), user("xan").Uid, info.Uid, info, olu); err != nil {
			t.Fatalf("hand the flyer to Yuri: %v", err)
		}
		close(model.letGo)
		eventually("Yuri, the flyer's assignee now, finds it", func() bool { return finds("yuri", "yankee-bravo", flyer) })
		if finds("xan", "yankee-bravo", flyer) {
			t.Error("Xan, whom the flyer went from, finds it")
		}
	})
}
