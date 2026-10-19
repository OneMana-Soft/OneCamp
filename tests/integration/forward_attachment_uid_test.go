//go:build integration
// +build integration

package integration_test

// A forward's files are the ones the forwarder added in the forward box: new
// files, never a node the graph already holds. The forward wrote each file
// the request carried as it came, uid included, so naming another member's
// file by its uid linked that file into the forward (where the forwarder and
// everyone in the destination could open it), and wrote the request's fields
// over it; a uid of any other node (a private doc, a project) was written over
// the same way. Against real Postgres, Redis and Dgraph.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAForwardNeverWritesANodeTheRequestNames -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAForwardNeverWritesANodeTheRequestNames(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Ana keeps a file in her private channel, which Ben isn't in, and a
	// private doc; both are in #open, where Ben forwards to.
	ana, ben := uuid.New(), uuid.New()
	private, open, anaPost, project := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'ana-only', true), ($2, 'open', false)`, private, open)
	for name, id := range map[string]uuid.UUID{"ana": ana, "ben": ben} {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
	}
	zero := "0001-01-01T00:00:00Z"
	got := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": ben.String(), "user_name": "ben"},
		{"uid": "_:private", "dgraph.type": "Channel", "ch_uuid": private.String(), "ch_name": "ana-only", "ch_private": true, "ch_deleted_at": zero,
			"ch_members": []map[string]any{{"uid": "_:ana"}},
			"ch_posts": []map[string]any{{"uid": "_:anapost", "dgraph.type": "Post", "post_uuid": anaPost.String(), "post_text": "<p>the contract</p>",
				"post_deleted_at": zero, "post_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
				"post_by": map[string]any{"uid": "_:ana"}, "post_channel": map[string]any{"uid": "_:private"},
				"post_attachments": []map[string]any{{"uid": "_:file", "dgraph.type": "Attachment", "attachment_uuid": uuid.NewString(),
					"attachment_file_name": "contract.pdf", "attachment_obj_key": "ana/contract.pdf", "attachment_created_by": map[string]any{"uid": "_:ana"}}}}}},
		{"uid": "_:open", "dgraph.type": "Channel", "ch_uuid": open.String(), "ch_name": "open", "ch_private": false, "ch_deleted_at": zero,
			"ch_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}}},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": "Salaries", "doc_private": true, "doc_deleted_at": zero,
			"doc_created_by": map[string]any{"uid": "_:ana"}},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Payroll"},
	})

	asBen := func() context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, ben)
		if err != nil || pg == nil {
			t.Fatalf("load ben: %v", err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, ben.String())
		if err != nil || gr == nil {
			t.Fatalf("load ben's node: %v", err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	forward := func(text string, files []map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"fwd_text":        text,
			"fwd_list":        []map[string]any{{"channel_uuid": open.String(), "channel_dgraph_uid": got["open"]}},
			"fwd_attachments": files,
		})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asBen()))
		if rec.Code != http.StatusOK {
			t.Fatalf("forwarding: %d %s", rec.Code, rec.Body.String())
		}
	}
	read := func(q string, out any) {
		t.Helper()
		if err := json.Unmarshal(dg.Query(t, q), out); err != nil {
			t.Fatalf("decoding %s: %v", q, err)
		}
	}
	type file struct {
		Uid  string `json:"uid"`
		Uuid string `json:"attachment_uuid"`
		Name string `json:"attachment_file_name"`
		Key  string `json:"attachment_obj_key"`
		By   *struct {
			Uid string `json:"uid"`
		} `json:"attachment_created_by"`
	}
	// filesIn is the files of the forward with text in #open.
	filesIn := func(text string) []file {
		t.Helper()
		var out struct {
			Ch []struct {
				Posts []struct {
					Files []file `json:"post_attachments"`
				} `json:"ch_posts"`
			} `json:"ch"`
		}
		read(fmt.Sprintf(`{ ch(func: uid(%s)) { ch_posts @filter(eq(post_text, %q)) { post_attachments { uid attachment_uuid attachment_file_name attachment_obj_key attachment_created_by { uid } } } } }`, got["open"], text), &out)
		if len(out.Ch) != 1 || len(out.Ch[0].Posts) != 1 {
			t.Fatalf("the forward %q is not in #open: %+v", text, out)
		}
		return out.Ch[0].Posts[0].Files
	}

	// 1. Ben names Ana's file by its uid, renamed, and a nested project.
	forward("<p>look</p>", []map[string]any{{"uid": got["file"], "attachment_uuid": uuid.NewString(), "attachment_file_name": "pwned.pdf",
		"attachment_obj_key": "ben/upload.pdf", "attachment_project": map[string]any{"uid": got["project"], "project_name": "pwned"}}})
	for _, f := range filesIn("<p>look</p>") {
		if f.Uid == got["file"] {
			t.Errorf("the forward links Ana's file %s, from a private channel Ben isn't in", got["file"])
		}
		if f.By == nil || f.By.Uid != got["ben"] {
			t.Errorf("a forwarded file is not Ben's: %+v", f)
		}
	}
	// 2. Ben names Ana's private doc as a file.
	forward("<p>again</p>", []map[string]any{{"uid": got["doc"], "attachment_uuid": uuid.NewString(), "attachment_file_name": "x.pdf", "attachment_obj_key": "ben/x.pdf"}})
	for _, f := range filesIn("<p>again</p>") {
		if f.Uid == got["doc"] {
			t.Errorf("the forward links Ana's private doc %s as a file", got["doc"])
		}
	}

	// Nothing the requests named changed.
	var after struct {
		File []struct {
			Name string `json:"attachment_file_name"`
			Key  string `json:"attachment_obj_key"`
		} `json:"file"`
		Holders []struct {
			Count int `json:"count"`
		} `json:"holders"`
		Doc []struct {
			Types []string `json:"dgraph.type"`
			Name  string   `json:"attachment_file_name"`
		} `json:"doc"`
		Project []struct {
			Name string `json:"project_name"`
		} `json:"project"`
	}
	read(fmt.Sprintf(`{
		file(func: uid(%[1]s)) { attachment_file_name attachment_obj_key }
		holders(func: has(post_uuid)) @filter(uid_in(post_attachments, %[1]s)) { count(uid) }
		doc(func: uid(%[2]s)) { dgraph.type attachment_file_name }
		project(func: uid(%[3]s)) { project_name }
	}`, got["file"], got["doc"], got["project"]), &after)
	if len(after.File) != 1 || after.File[0].Name != "contract.pdf" || after.File[0].Key != "ana/contract.pdf" {
		t.Errorf("Ana's file changed: %+v (want contract.pdf at ana/contract.pdf)", after.File)
	}
	if len(after.Holders) != 1 || after.Holders[0].Count != 1 {
		t.Errorf("Ana's file is in %+v posts, want her own only", after.Holders)
	}
	if len(after.Doc) != 1 || len(after.Doc[0].Types) != 1 || after.Doc[0].Types[0] != "Doc" || after.Doc[0].Name != "" {
		t.Errorf("Ana's doc changed: %+v", after.Doc)
	}
	if len(after.Project) != 1 || after.Project[0].Name != "Payroll" {
		t.Errorf("the project changed: %+v", after.Project)
	}

	// 3. A file Ben added in the forward box still goes with it, as his.
	added := uuid.NewString()
	forward("<p>with a file</p>", []map[string]any{{"attachment_uuid": added, "attachment_file_name": "notes.pdf", "attachment_obj_key": "ben/notes.pdf", "attachment_type": "document"}})
	files := filesIn("<p>with a file</p>")
	if len(files) != 1 || files[0].Uuid != added || files[0].Name != "notes.pdf" || files[0].Key != "ben/notes.pdf" || files[0].By == nil || files[0].By.Uid != got["ben"] {
		t.Errorf("Ben's own file: %+v, want notes.pdf (%s) as his", files, added)
	}
}
