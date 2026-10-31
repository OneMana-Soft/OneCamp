//go:build integration
// +build integration

package integration_test

// A file is the file of the place it is uploaded for (src_key, src_value), and
// is served to whoever may see that place. POST /uploadFile checked that only
// for a doc: anyone signed in could add a file to a channel, a conversation, a
// project or a board they weren't in, by naming it, and an upload for several
// places at once (channel_uuids, chat_uuids) took any. Each place's own rule
// now decides (business/FileAccess), for an upload and for every route that
// hands a file out.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOnlyThoseWhoMayWriteInAPlaceAddFilesToIt|TestOnlyThoseWhoMaySeeAPlaceHaveItsFiles' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// placesForFiles is a workspace with a place of every kind, all Mia's:
// a private channel, an archived one, a group with Ann and Ben, a project,
// and Ann's private board, which Mia may change and Oli may only read. Oli is
// in none of them; Zed is in nothing at all.
type placesForFiles struct {
	env                              *integration.Env
	dg                               *integration.DgraphEnv
	people                           map[string]uuid.UUID
	uids                             map[string]string
	channel, archived, project       string
	board, group, dmMiaAnn, dmAnnOli string
}

func setUpPlacesForFiles(t *testing.T) *placesForFiles {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)

	p := &placesForFiles{env: env, dg: dg, people: map[string]uuid.UUID{}}
	var nodes []map[string]any
	for _, name := range []string{"mia", "oli", "ann", "ben", "zed"} {
		id := uuid.New()
		p.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	group, err := helpers.GenerateGroupID([]string{p.people["mia"].String(), p.people["ann"].String(), p.people["ben"].String()})
	if err != nil {
		t.Fatal(err)
	}
	p.channel, p.archived, p.project, p.board, p.group = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), group
	p.dmMiaAnn = helpers.GetGroupingId(p.people["mia"].String(), p.people["ann"].String())
	p.dmAnnOli = helpers.GetGroupingId(p.people["ann"].String(), p.people["oli"].String())
	live := "0001-01-01T00:00:00Z"
	mia := []map[string]any{{"uid": "_:mia"}}
	nodes = append(nodes,
		map[string]any{"uid": "_:finance", "dgraph.type": "Channel", "ch_uuid": p.channel, "ch_name": "finance",
			"ch_private": true, "ch_deleted_at": live, "ch_members": mia},
		map[string]any{"uid": "_:old", "dgraph.type": "Channel", "ch_uuid": p.archived, "ch_name": "old",
			"ch_private": true, "ch_deleted_at": "2026-01-01T00:00:00Z", "ch_members": mia},
		map[string]any{"uid": "_:crew", "dgraph.type": "Dm", "dm_grouping_id": p.group,
			"dm_participants": []map[string]any{{"uid": "_:mia"}, {"uid": "_:ann"}, {"uid": "_:ben"}}},
		map[string]any{"uid": "_:audit", "dgraph.type": "Project", "project_uuid": p.project, "project_name": "audit",
			"project_members": mia},
		map[string]any{"uid": "_:plans", "dgraph.type": "Board", "board_uuid": p.board, "board_title": "Plans",
			"board_private": true, "board_deleted_at": live, "board_created_by": map[string]any{"uid": "_:ann"},
			"board_editing_users": mia, "board_reading_users": []map[string]any{{"uid": "_:oli"}}},
	)
	p.uids = dg.Mutate(t, nodes)
	return p
}

// as is the person as a signed-in request carries them.
func (p *placesForFiles) as(name string) userModels.UserInfo {
	var u userModels.UserInfo
	u.UserPostgresInfo.Id = p.people[name]
	u.UserPostgresInfo.EmailID = p.people[name].String() + "@example.test"
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: p.uids[name], Uuid: p.people[name].String(), UserName: name}
	return u
}

// upload posts a file with jsonData as /uploadFile's sign-in leaves the
// request: the Postgres account only.
func (p *placesForFiles) upload(t *testing.T, who string, jsonData string) (int, map[string]any) {
	t.Helper()
	return uploadAs(t, p.as(who).UserPostgresInfo, "plan.txt", []byte("the plan"), jsonData)
}

// uploadAs posts one file, name with content, to /uploadFile as who, with
// jsonData, as the route's sign-in (VerifyAuthOnlyPostgres) leaves the
// request: the Postgres account only. It answers the status and the body.
func uploadAs(t *testing.T, who userModels.User, name string, content []byte, jsonData string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", name)
	_, _ = part.Write(content)
	_ = mw.WriteField("jsonData", jsonData)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploadFile", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var u userModels.UserInfo
	u.UserPostgresInfo = who
	req = req.WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, u))
	rec := httptest.NewRecorder()
	userController.UploadFile(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// stored is how many files the person has stored.
func (p *placesForFiles) stored(t *testing.T, who string) int {
	t.Helper()
	var n int
	if err := p.env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE created_by = $1`, p.people[who]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func place(kind, id string) string {
	return `{"src_key":"` + kind + `","src_value":"` + id + `"}`
}

func TestOnlyThoseWhoMayWriteInAPlaceAddFilesToIt(t *testing.T) {
	p := setUpPlacesForFiles(t)
	str := func(name string) string { return p.people[name].String() }
	fresh := func(names ...string) string {
		var ids []string
		for _, n := range names {
			ids = append(ids, str(n))
		}
		id, err := helpers.GenerateGroupID(ids)
		if err != nil {
			t.Fatal(err)
		}
		people, _ := json.Marshal(ids)
		return `{"src_key":"grpChat","src_value":"` + id + `","participants":` + string(people) + `}`
	}
	kinds := []struct{ name, jsonData string }{
		{"a channel", place("channel", p.channel)},
		{"a conversation", place("chat", p.dmMiaAnn)},
		{"a group", place("grpChat", p.group)},
		{"a project", place("project", p.project)},
		{"a board", place("board", p.board)},
		{"a channel, sent to several places at once", `{"src_key":"chat","channel_uuids":["` + p.channel + `"]}`},
		{"a conversation, sent to several places at once", `{"src_key":"chat","chat_uuids":["` + p.dmMiaAnn + `"]}`},
	}
	for _, k := range kinds {
		t.Run("someone in "+k.name+" adds a file to it", func(t *testing.T) {
			before := p.stored(t, "mia")
			if code, body := p.upload(t, "mia", k.jsonData); code != http.StatusOK || body["object_uuid"] == nil {
				t.Fatalf("%d %v", code, body)
			}
			if p.stored(t, "mia") != before+1 {
				t.Fatal("the file wasn't stored")
			}
		})
		t.Run("someone not in "+k.name+" is refused", func(t *testing.T) {
			before := p.stored(t, "oli")
			code, body := p.upload(t, "oli", k.jsonData)
			if code != http.StatusForbidden || body["msg"] != "You can't add files here." {
				t.Fatalf("%d %v", code, body)
			}
			if p.stored(t, "oli") != before {
				t.Fatal("a refused file was stored")
			}
		})
	}

	refused := []struct{ name, who, jsonData string }{
		{"an archived channel, by its member", "mia", place("channel", p.archived)},
		{"a channel that isn't there", "mia", place("channel", uuid.NewString())},
		{"a board, by someone who may only read it", "oli", place("board", p.board)},
		{"a kind with no rule (an import's task)", "mia", place("task", p.project)},
		{"a new group of others, named by its people", "oli", fresh("mia", "ann", "zed")},
		{"several places at once, one of them not the uploader's", "mia", `{"src_key":"chat","channel_uuids":["` + p.channel + `","` + p.archived + `"]}`},
	}
	for _, c := range refused {
		t.Run(c.name+" is refused", func(t *testing.T) {
			before := p.stored(t, c.who)
			code, body := p.upload(t, c.who, c.jsonData)
			if code != http.StatusForbidden || body["msg"] != "You can't add files here." {
				t.Fatalf("%d %v", code, body)
			}
			if p.stored(t, c.who) != before {
				t.Fatal("a refused file was stored")
			}
		})
	}

	// A group is written at its first message; the web makes its id from its
	// people first, so its first message can carry a file.
	t.Run("a new group's file, named by its people, the uploader among them", func(t *testing.T) {
		if code, body := p.upload(t, "oli", fresh("oli", "ann", "ben")); code != http.StatusOK {
			t.Fatalf("%d %v", code, body)
		}
	})
	t.Run("anyone adds a public file (a profile picture)", func(t *testing.T) {
		if code, body := p.upload(t, "zed", `{"src_key":"public"}`); code != http.StatusOK {
			t.Fatalf("%d %v", code, body)
		}
	})
}

func TestOnlyThoseWhoMaySeeAPlaceHaveItsFiles(t *testing.T) {
	p := setUpPlacesForFiles(t)
	files := map[string]string{}
	for kind, at := range map[string]string{"channel": p.channel, "chat": p.dmMiaAnn, "grpChat": p.group, "project": p.project, "board": p.board} {
		id := uuid.NewString()
		files[kind] = id
		if _, err := p.env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			id, p.people["mia"].String()+"/"+id+"_plan.png", kind, at, p.people["mia"]); err != nil {
			t.Fatal(err)
		}
	}
	get := func(handler http.HandlerFunc, who string, params map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx := context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, p.as(who))
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		return rec
	}
	type fetch struct {
		name    string
		handler http.HandlerFunc
		who     string
		params  map[string]string
		want    int
	}
	ann, mia := p.people["ann"].String(), p.people["mia"].String()
	cases := []fetch{
		{"a private channel's file, by its member", userController.GetChannelFile, "mia", map[string]string{"channel_uuid": p.channel, "obj_uuid": files["channel"]}, http.StatusOK},
		{"a private channel's file, by someone not in it", userController.GetChannelFile, "oli", map[string]string{"channel_uuid": p.channel, "obj_uuid": files["channel"]}, http.StatusForbidden},
		{"a conversation's file, by one of its two", userController.GetChatFile, "mia", map[string]string{"chat_uuid": ann, "obj_uuid": files["chat"]}, http.StatusOK},
		{"a conversation's file, by someone else, through their own conversation with one of them", userController.GetChatFile, "oli", map[string]string{"chat_uuid": ann, "obj_uuid": files["chat"]}, http.StatusNotFound},
		{"a conversation's file, by someone else, through the other one", userController.GetChatFile, "oli", map[string]string{"chat_uuid": mia, "obj_uuid": files["chat"]}, http.StatusNotFound},
		{"a group's file, by one of its people", userController.GetGroupChatFile, "ann", map[string]string{"grp_id": p.group, "obj_uuid": files["grpChat"]}, http.StatusOK},
		{"a group's file, by someone not in it", userController.GetGroupChatFile, "oli", map[string]string{"grp_id": p.group, "obj_uuid": files["grpChat"]}, http.StatusForbidden},
		{"a project's file, by its member", userController.GetProjectFile, "mia", map[string]string{"project_uuid": p.project, "obj_uuid": files["project"]}, http.StatusOK},
		{"a project's file, by someone not in it", userController.GetProjectFile, "oli", map[string]string{"project_uuid": p.project, "obj_uuid": files["project"]}, http.StatusForbidden},
		{"a private board's picture, by someone it's shared with to read", userController.GetBoardAttachment, "oli", map[string]string{"board_uuid": p.board, "obj_uuid": files["board"]}, http.StatusTemporaryRedirect},
		{"a private board's picture, by someone it isn't shared with", userController.GetBoardAttachment, "zed", map[string]string{"board_uuid": p.board, "obj_uuid": files["board"]}, http.StatusForbidden},
		{"a channel's file asked for as a project's, by someone in both", userController.GetProjectFile, "mia", map[string]string{"project_uuid": p.project, "obj_uuid": files["channel"]}, http.StatusNotFound},
		{"a project's file asked for under a channel's id", userController.GetChannelFile, "mia", map[string]string{"channel_uuid": p.channel, "obj_uuid": files["project"]}, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(c.handler, c.who, c.params)
			if rec.Code != c.want {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, c.want)
			}
			gaveURL := bytes.Contains(rec.Body.Bytes(), []byte(`"url"`)) || rec.Header().Get("Location") != ""
			if ok := c.want == http.StatusOK || c.want == http.StatusTemporaryRedirect; gaveURL != ok {
				t.Fatalf("handed out an address: %v (%s)", gaveURL, rec.Body)
			}
		})
	}
}
