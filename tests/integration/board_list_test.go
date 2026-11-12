//go:build integration
// +build integration

package integration_test

// GET /board/getBoardList?editable=1 lists only the boards the caller may
// edit (CanEdit: their own, and those shared with them to edit). A call's
// "Present a board" picker asks for it, because the server presents no other
// board (board_not_editable): a board shared with someone to read or to
// comment on is never offered to them. Without the flag the list is every
// board shared with them, as before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheBoardListNarrowsToTheBoardsYouMayEdit -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardController "github.com/akashc777/OneCamp/controllers/Board"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	entityLinkDomain "github.com/akashc777/OneCamp/domain/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheBoardListNarrowsToTheBoardsYouMayEdit(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-list-editable-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me, them := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"me": me, "them": them} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.com", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	board := boardSeeder(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC))
	mine, edit, read, comment := board("mine", "Roadmap mine", "me"), board("edit", "Roadmap edit", "them"),
		board("read", "Roadmap read", "them"), board("comment", "Roadmap comment", "them")
	edit["board_editing_users"] = []map[string]any{{"uid": "_:me"}}
	read["board_reading_users"] = []map[string]any{{"uid": "_:me"}}
	comment["board_commenting_users"] = []map[string]any{{"uid": "_:me"}}
	unshared := board("unshared", "Roadmap unshared", "them")
	gone := board("gone", "Roadmap gone", "me")
	gone["board_deleted_at"] = gone["board_updated_at"]
	dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me"},
		{"uid": "_:them", "dgraph.type": "User", "user_uuid": them.String(), "user_name": "them"},
		mine, edit, read, comment, unshared, gone,
	})

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/board/getBoardList", boardController.GetBoardList)
	type page = boardPage
	list := func(t *testing.T, as uuid.UUID, query string) page {
		t.Helper()
		return listBoardsOK(t, r, as, query)
	}
	want := wantBoardPage

	t.Run("without the flag, every board shared with them is listed, newest edited first", func(t *testing.T) {
		want(t, list(t, me, ""), page{titles: []string{"Roadmap comment", "Roadmap read", "Roadmap edit", "Roadmap mine"}, count: 4, pageCount: 1})
	})
	t.Run("with editable=1, a board they may only read or comment on is left out", func(t *testing.T) {
		want(t, list(t, me, "editable=1"), page{titles: []string{"Roadmap edit", "Roadmap mine"}, count: 2, pageCount: 1})
	})
	t.Run("the count and the pages are the narrowed list's", func(t *testing.T) {
		want(t, list(t, me, "editable=1&pageSize=1"), page{titles: []string{"Roadmap edit"}, count: 2, pageCount: 2})
		want(t, list(t, me, "editable=1&pageSize=1&pageIndex=1"), page{titles: []string{"Roadmap mine"}, count: 2, pageCount: 2})
	})
	t.Run("a search narrows the editable list further", func(t *testing.T) {
		want(t, list(t, me, "editable=1&search=edit"), page{titles: []string{"Roadmap edit"}, count: 1, pageCount: 1})
		want(t, list(t, me, "editable=1&search=read"), page{titles: []string{}, count: 0, pageCount: 1})
	})
	t.Run("the owner of the shared boards may edit all of them", func(t *testing.T) {
		want(t, list(t, them, "editable=1"), page{titles: []string{"Roadmap unshared", "Roadmap comment", "Roadmap read", "Roadmap edit"}, count: 4, pageCount: 1})
	})
}

// GET /board/getBoardList?owner=mine|shared&project=<uuid>|team=<uuid>
// (miro-46: the boards list, spaces and finding a board) narrows the list of
// every board the caller may read, and never widens it: mine is the boards
// they made; shared is the rest of what they may read; project is the boards
// linked from that project's home (EntityLink's linked_boards); team is the
// boards linked from any of the team's live projects, as the team page lists
// them. An archived project lists its boards to whoever may open it; deleted
// teams, deleted boards and removed links never count, and the count and
// pages are the narrowed list's. A project the caller may not view, or a
// team they may not read (member, team admin or workspace admin), answers
// 404, as one that isn't there does, so the answer never says it exists; a
// workspace admin outside a team still lists only the boards they may read.
// A project and a team at once, an owner it doesn't know or an id that isn't
// one is 400.
//
// Run: go test -tags=integration ./tests/integration/ -run TestBoardListFilters -v
func TestBoardListFilters(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-list-filters-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me, them, outsider, boss := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"me": me, "them": them, "outsider": outsider, "boss": boss} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.com", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// boss is a workspace admin in no team and no project.
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`, boss); err != nil {
		t.Fatalf("make boss a workspace admin: %v", err)
	}
	launch, budget, archived, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ops, finance, old := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"Launch": launch, "Budget": budget, "Archived": archived, "Secret": secret} {
		if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, id, name, them); err != nil {
			t.Fatalf("seed project %s: %v", name, err)
		}
	}

	board := boardSeeder(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	minePlain, mineLinked := board("minePlain", "Mine plain", "me"), board("mineLinked", "Mine linked", "me")
	sharedLinked, sharedPlain := board("sharedLinked", "Shared linked", "them"), board("sharedPlain", "Shared plain", "them")
	unshared, gone := board("unshared", "Unshared linked", "them"), board("gone", "Gone linked", "me")
	budgetBoard, archivedBoard := board("budgetBoard", "Budget board", "them"), board("archivedBoard", "Archived board", "me")
	unlinked, secretBoard := board("unlinked", "Unlinked later", "me"), board("secretBoard", "Secret board", "them")
	// As CreateBoard does, the maker also edits each board they made: owner=shared's `not uid(owned)` is then the only
	// thing keeping a person's own boards out of what is shared with them (CR-231 row 2767 F5).
	for _, mine := range []map[string]any{minePlain, mineLinked, gone, archivedBoard, unlinked} {
		mine["board_editing_users"] = refs("me")
	}
	sharedLinked["board_editing_users"] = refs("me")
	sharedPlain["board_reading_users"] = refs("me")
	budgetBoard["board_commenting_users"] = refs("me", "boss")
	secretBoard["board_reading_users"] = refs("me", "boss")
	gone["board_deleted_at"] = gone["board_updated_at"]

	const live = "0001-01-01T00:00:00Z"
	// A project's deleted_at is its archive mark (ArchiveProjectByProjectUUID):
	// its members still open it, to restore it.
	deletedAt := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	project := func(node, name string, id uuid.UUID, deleted string, team string, members []string, boards ...string) map[string]any {
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Project", "project_uuid": id.String(), "project_name": name,
			"project_deleted_at": deleted, "project_members": refs(members...), "project_team": map[string]any{"uid": "_:" + team},
			"linked_boards": refs(boards...)}
	}
	team := func(node, name string, id uuid.UUID, deleted string, members []string, projects ...string) map[string]any {
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Team", "team_uuid": id.String(), "team_name": name,
			"team_deleted_at": deleted, "team_members": refs(members...), "team_projects": refs(projects...)}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me"},
		{"uid": "_:them", "dgraph.type": "User", "user_uuid": them.String(), "user_name": "them"},
		{"uid": "_:outsider", "dgraph.type": "User", "user_uuid": outsider.String(), "user_name": "outsider"},
		{"uid": "_:boss", "dgraph.type": "User", "user_uuid": boss.String(), "user_name": "boss"},
		minePlain, mineLinked, sharedLinked, sharedPlain, unshared, gone, budgetBoard, archivedBoard, unlinked, secretBoard,
		project("launch", "Launch", launch, live, "ops", []string{"me", "them"}, "mineLinked", "sharedLinked", "unshared", "gone", "unlinked"),
		project("budget", "Budget", budget, live, "ops", []string{"me"}, "budgetBoard"),
		project("archived", "Archived", archived, deletedAt, "ops", []string{"me"}, "archivedBoard"),
		project("secret", "Secret", secret, live, "finance", []string{"them"}, "secretBoard"),
		team("ops", "Ops", ops, live, []string{"me", "them"}, "launch", "budget", "archived"),
		team("finance", "Finance", finance, live, []string{"them"}, "secret"),
		team("old", "Old", old, deletedAt, []string{"me"}, "launch"),
	})

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/board/getBoardList", boardController.GetBoardList)
	r.Get("/team/info/{team_uuid}", teamController.GetTeamInfo)
	r.Get("/team/membersInfo/{team_uuid}", teamController.GetDgraphTeamMemberListByTeamUUID)
	list := func(t *testing.T, as uuid.UUID, query string) boardPage {
		t.Helper()
		return listBoardsOK(t, r, as, query)
	}
	titles := func(ts ...string) boardPage {
		return boardPage{titles: append([]string{}, ts...), count: uint64(len(ts)), pageCount: 1}
	}

	t.Run("no filter: every board they may read, as before", func(t *testing.T) {
		wantBoardPage(t, list(t, me, ""), titles("Secret board", "Unlinked later", "Archived board", "Budget board", "Shared plain", "Shared linked", "Mine linked", "Mine plain"))
	})
	t.Run("owner=mine: the boards they made", func(t *testing.T) {
		wantBoardPage(t, list(t, me, "owner=mine"), titles("Unlinked later", "Archived board", "Mine linked", "Mine plain"))
	})
	t.Run("owner=shared: the boards shared with them, less their own", func(t *testing.T) {
		wantBoardPage(t, list(t, me, "owner=shared"), titles("Secret board", "Budget board", "Shared plain", "Shared linked"))
	})
	t.Run("project: its linked boards they may read; unlinked, unreadable and deleted boards are left out", func(t *testing.T) {
		q := "project=" + launch.String()
		wantBoardPage(t, list(t, me, q), titles("Unlinked later", "Shared linked", "Mine linked"))
		wantBoardPage(t, list(t, me, q+"&owner=mine"), titles("Unlinked later", "Mine linked"))
		wantBoardPage(t, list(t, me, q+"&owner=shared"), titles("Shared linked"))
		wantBoardPage(t, list(t, me, q+"&search=shared"), titles("Shared linked"))
		// Another member reads the same project through their own boards.
		wantBoardPage(t, list(t, them, q), titles("Unshared linked", "Shared linked"))
	})
	t.Run("a link removed from the project drops its board", func(t *testing.T) {
		if err := entityLinkDomain.RemoveLink(ctx, entityLinkDomain.SourceProject, launch.String(), entityLinkDomain.RefBoard, unlinked["board_uuid"].(string)); err != nil {
			t.Fatal(err)
		}
		wantBoardPage(t, list(t, me, "project="+launch.String()), titles("Shared linked", "Mine linked"))
	})
	t.Run("the count and the pages are the filtered list's", func(t *testing.T) {
		q := "project=" + launch.String() + "&pageSize=1"
		wantBoardPage(t, list(t, me, q), boardPage{titles: []string{"Shared linked"}, count: 2, pageCount: 2})
		wantBoardPage(t, list(t, me, q+"&pageIndex=1"), boardPage{titles: []string{"Mine linked"}, count: 2, pageCount: 2})
	})
	t.Run("an archived project lists its linked boards to whoever may open it", func(t *testing.T) {
		wantBoardPage(t, list(t, me, "project="+archived.String()), titles("Archived board"))
	})
	t.Run("team: the boards linked from its live projects, as the team page lists them; the archived project's are left out", func(t *testing.T) {
		q := "team=" + ops.String()
		wantBoardPage(t, list(t, me, q), titles("Budget board", "Shared linked", "Mine linked"))
		wantBoardPage(t, list(t, me, q+"&owner=shared"), titles("Budget board", "Shared linked"))
		wantBoardPage(t, list(t, me, q+"&owner=mine"), titles("Mine linked"))
	})
	t.Run("a workspace admin outside a team lists only the boards linked from it that they may read", func(t *testing.T) {
		wantBoardPage(t, list(t, boss, "team="+ops.String()), titles("Budget board"))
		wantBoardPage(t, list(t, boss, "team="+finance.String()), titles("Secret board"))
		wantBoardPage(t, list(t, boss, "team="+ops.String()+"&owner=mine"), titles())
	})
	t.Run("the team's own pages admit whoever its filter admits: a member and a workspace admin, not an outsider", func(t *testing.T) {
		for _, path := range []string{"/team/info/", "/team/membersInfo/"} {
			for _, c := range []struct {
				name string
				as   uuid.UUID
				want int
			}{
				{"a member", me, http.StatusOK},
				{"a workspace admin outside it", boss, http.StatusOK},
				{"someone outside it", outsider, http.StatusForbidden},
			} {
				if rec := getAs(t, r, c.as, path+ops.String()); rec.Code != c.want {
					t.Errorf("%sOps for %s: %d %s, want %d", path, c.name, rec.Code, rec.Body.String(), c.want)
				}
			}
		}
	})
	t.Run("a project or team they may not read, a deleted team and a missing one are all 404, alike", func(t *testing.T) {
		missingProject, missingTeam := "", ""
		for _, c := range []struct {
			name  string
			as    uuid.UUID
			query string
		}{
			{"a project that isn't there", me, "project=" + uuid.NewString()},
			{"a project they aren't in, though a board linked from it is shared with them", me, "project=" + secret.String()},
			{"an archived project they aren't in", outsider, "project=" + archived.String()},
			{"someone in no project", outsider, "project=" + launch.String()},
			{"a team that isn't there", me, "team=" + uuid.NewString()},
			{"a team they aren't in", me, "team=" + finance.String()},
			{"a deleted team", me, "team=" + old.String()},
			{"someone in no team", outsider, "team=" + ops.String()},
			{"a team that isn't there, for a workspace admin", boss, "team=" + uuid.NewString()},
			{"a deleted team, for a workspace admin", boss, "team=" + old.String()},
		} {
			code, _, body := listBoards(t, r, c.as, c.query)
			if code != http.StatusNotFound {
				t.Errorf("%s: %d %s, want 404", c.name, code, body)
				continue
			}
			missing := &missingProject
			if strings.HasPrefix(c.query, "team=") {
				missing = &missingTeam
			}
			if *missing == "" {
				*missing = body
			} else if body != *missing {
				t.Errorf("%s: answered %s, unlike one that isn't there: %s", c.name, body, *missing)
			}
		}
	})
	t.Run("a project and a team at once, an unknown owner or an id that isn't one is 400", func(t *testing.T) {
		for _, q := range []string{
			"project=" + launch.String() + "&team=" + ops.String(),
			"owner=everyone",
			"project=launch",
			"team=ops",
		} {
			if code, _, body := listBoards(t, r, me, q); code != http.StatusBadRequest {
				t.Errorf("%s: %d %s, want 400", q, code, body)
			}
		}
	})
}

// refs is dgraph.Mutate's list of edges to the blank nodes named.
func refs(nodes ...string) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, map[string]any{"uid": "_:" + n})
	}
	return out
}

// boardSeeder makes live private boards for dg.Mutate, owned by the blank
// user node named, each edited a minute after the one before it from start.
func boardSeeder(start time.Time) func(node, title, owner string) map[string]any {
	edited := start
	return func(node, title, owner string) map[string]any {
		edited = edited.Add(time.Minute)
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Board", "board_uuid": uuid.NewString(), "board_title": title,
			"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_updated_at": edited.Format(time.RFC3339),
			"board_created_by": map[string]any{"uid": "_:" + owner}}
	}
}

// boardPage is one answer of the board list: the boards' titles in the order
// listed, with the count and the number of pages.
type boardPage struct {
	titles    []string
	count     uint64
	pageCount uint64
}

// listBoards asks h for the board list as a person: the status, the page
// when it is a 200, and the body.
func listBoards(t *testing.T, h http.Handler, as uuid.UUID, query string) (int, boardPage, string) {
	t.Helper()
	rec := getAs(t, h, as, "/board/getBoardList?"+query)
	p := boardPage{titles: []string{}}
	if rec.Code != http.StatusOK {
		return rec.Code, p, rec.Body.String()
	}
	var got struct {
		PageCount uint64                       `json:"pageCount"`
		Data      dgraphStruct.DgraphBoardList `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	p.count, p.pageCount = got.Data.Count, got.PageCount
	for _, b := range got.Data.Boards {
		p.titles = append(p.titles, b.Title)
	}
	return rec.Code, p, rec.Body.String()
}

// getAs asks h for target as a person signed in (their session cookie).
func getAs(t *testing.T, h http.Handler, as uuid.UUID, target string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
	req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// listBoardsOK is listBoards for an answer that must be a 200.
func listBoardsOK(t *testing.T, h http.Handler, as uuid.UUID, query string) boardPage {
	t.Helper()
	code, p, body := listBoards(t, h, as, query)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", query, code, body)
	}
	return p
}

func wantBoardPage(t *testing.T, got, want boardPage) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
}
