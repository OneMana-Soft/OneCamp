//go:build integration
// +build integration

package integration_test

// A deleted imported message's files open for nobody, as a message written in
// the app does (deleted_message_files_test.go): an imported channel post's, a
// conversation message's and a thread reply's. So does a task comment's file
// an import kept, and the purge takes its row.
//
// The import file stages wrote each file twice: first a node of its own, then
// the node the item it recorded as the file's holder lists (import_id_map's
// parent_kind and parent_uuid), both with the same time and a maker that is a
// user node of its own holding the importer's id (business/SlackImport
// file_worker, with the export's message time; business/Import
// attachment_worker, with the import's). The first kept the file after its
// item was deleted: its maker's id is the file's uploader's, and an export's
// time is older than 19 Oct. It holds nothing now that the listed node is
// stamped deleted, whichever item went. Nothing else loses the file: a forward
// of an imported message, by the file's uploader within its place, keeps it
// after the message goes, and takes it when it goes too; an imported message
// still there keeps its file when a forward of it goes; a reply sent to its
// channel too keeps it while either shows it; and a message from before 19
// Oct naming the file with its node's very time, of an import made before
// then, keeps it as any message from then does.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestADeletedImportedMessagesFilesOpenForNobody|TestARepairTakesTheFilesOfImportedMessagesDeletedBefore|TestADeletedImportedTaskCommentsFileGoes)$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// importedFiles are the files of setUpImportedFilesWorld, by name.
type importedFiles struct{ pitch, logo, plan, deck, specs, brief, memo heldFile }

// setUpImportedFilesWorld: setUpPlacesForFiles, where Mia imported her team's
// Slack: the pitch on a channel post, the logo on a message of her
// conversation with Ann, the plan on a thread reply in the channel, the deck
// and the specs on posts Mia then forwarded within the channel, the memo on a
// post whose file she then replied with in a thread and sent to the channel
// too, and the brief, imported before 19 Oct, on a post Ann named it in on 15
// Oct. Each file is written as an import's file stage writes it: Mia's row,
// the import's map of it naming the item holding it, and two nodes.
func setUpImportedFilesWorld(t *testing.T) (*filesWorld, importedFiles) {
	t.Helper()
	p := setUpPlacesForFiles(t)
	w := &filesWorld{placesForFiles: p, t: t, ids: map[string]string{}, graph: map[string]string{}}
	mia := p.people["mia"]
	inChannel := func(name string) heldFile { return heldFile{name, uuid.NewString(), "channel", p.channel} }
	f := importedFiles{pitch: inChannel("pitch.pdf"), logo: heldFile{"logo.png", uuid.NewString(), "chat", p.dmMiaAnn},
		plan: inChannel("plan.pdf"), deck: inChannel("deck.pdf"), specs: inChannel("specs.pdf"), brief: inChannel("brief.pdf"),
		memo: inChannel("memo.pdf")}
	w.all = []heldFile{f.pitch, f.logo, f.plan, f.deck, f.specs, f.brief, f.memo}
	for _, name := range []string{"pitchPost", "logoMsg", "threadPost", "planReply", "deckPost", "deckForward", "specsPost",
		"specsForward", "briefPost", "annsBriefPost", "memoPost", "memoReply", "memoCopy"} {
		w.ids[name] = uuid.NewString()
	}

	// Mia's import made each file's row (business/Attachment CreateAttachment)
	// now, the brief's on 10 Oct, and its map names each file it made with the
	// item holding it.
	job := uuid.NewString()
	w.exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by) VALUES ($1, 'slack', 'acme', 'export_zip', 'completed', $2)`, job, mia)
	holders := map[heldFile][2]string{f.pitch: {"post", "pitchPost"}, f.logo: {"chat", "logoMsg"}, f.plan: {"comment", "planReply"},
		f.deck: {"post", "deckPost"}, f.specs: {"post", "specsPost"}, f.brief: {"post", "briefPost"}, f.memo: {"post", "memoPost"}}
	for i, x := range w.all {
		made := "NOW()"
		if x == f.brief {
			made = "'2026-10-10T08:00:00Z'"
		}
		w.exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by, created_at) VALUES ($1, $2, $3, $4, $5, `+made+`)`,
			x.id, mia.String()+"/"+x.id+"_"+x.name, x.kind, x.place, mia)
		meta, _ := json.Marshal(map[string]string{"name": x.name, "parent_kind": holders[x][0], "parent_uuid": w.ids[holders[x][1]]})
		w.exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, metadata) VALUES ($1, 'file', $2, $3, $4)`,
			job, fmt.Sprintf("F0%d", i), x.id, string(meta))
	}

	live := "0001-01-01T00:00:00Z"
	ref := func(uid string) map[string]any { return map[string]any{"uid": uid} }
	byMia, byAnn := ref(p.uids["mia"]), ref(p.uids["ann"])
	// Each message's time, from the export: Slack's ts, to the microsecond.
	exported := map[heldFile]string{f.pitch: "2025-03-04T10:15:00.123456Z", f.logo: "2025-03-04T11:02:41.000210Z",
		f.plan: "2025-03-05T09:30:12.774001Z", f.deck: "2025-04-01T16:45:09.300120Z", f.specs: "2025-04-02T08:01:55.090807Z",
		f.brief: "2025-02-11T14:20:33.456789Z", f.memo: "2025-05-06T12:00:01.000001Z"}
	// A node as the file stage writes it: its message's time, its maker by
	// Mia's id alone, which makes a user node of its own.
	imported := func(x heldFile) map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": x.id, "attachment_file_name": x.name,
			"attachment_obj_key": mia.String() + "/" + x.id + "_" + x.name, "attachment_created_at": exported[x],
			"attachment_created_by": map[string]any{"user_uuid": mia.String()}}
	}
	// A node as the app writes it (models/dgraph NewAttachments), by who at at.
	written := func(x heldFile, who map[string]any, at string) map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": x.id, "attachment_file_name": x.name,
			"attachment_obj_key": mia.String() + "/" + x.id + "_" + x.name, "attachment_deleted_at": live,
			"attachment_created_at": at, "attachment_created_by": who}
	}
	post := func(name string, by map[string]any, at string, files ...map[string]any) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "Post", "post_uuid": w.ids[name], "post_by": by,
			"post_channel": ref(p.uids["finance"]), "post_created_at": at, "post_deleted_at": live, "post_attachments": files}
	}
	keep := func(uids map[string]string) {
		for k, v := range uids {
			w.graph[k] = v
		}
	}
	// The file stage's first node of each file, which nothing lists.
	var loose []map[string]any
	for _, x := range w.all {
		loose = append(loose, imported(x))
	}
	p.dg.Mutate(t, loose)
	// Then the items, with the nodes they list; Mia's forwards since; her
	// reply in the thread with the memo, sent to the channel too.
	now := time.Now().UTC()
	forwardedAt, repliedAt := now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(-30*time.Minute).Format(time.RFC3339Nano)
	threadPost := post("threadPost", byMia, "2025-03-05T09:00:00Z")
	threadPost["post_comments"] = []map[string]any{
		{"uid": "_:planReply", "dgraph.type": "Comment", "comment_uuid": w.ids["planReply"], "comment_by": byMia,
			"comment_post": ref("_:threadPost"), "comment_created_at": exported[f.plan], "comment_deleted_at": live,
			"comment_attachments": []map[string]any{imported(f.plan)}},
		{"uid": "_:memoReply", "dgraph.type": "Comment", "comment_uuid": w.ids["memoReply"], "comment_by": byMia,
			"comment_post": ref("_:threadPost"), "comment_created_at": repliedAt, "comment_deleted_at": live,
			"comment_attachments": []map[string]any{written(f.memo, byMia, repliedAt)}},
	}
	memoCopy := post("memoCopy", byMia, repliedAt, written(f.memo, byMia, repliedAt))
	memoCopy["post_broadcast_of"] = ref("_:memoReply")
	keep(p.dg.Mutate(t, []map[string]any{
		{"uid": p.uids["finance"], "ch_members": []map[string]any{byAnn}},
		post("pitchPost", byMia, exported[f.pitch], imported(f.pitch)),
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": p.dmMiaAnn, "dm_participants": []map[string]any{byMia, byAnn}},
		{"uid": "_:logoMsg", "dgraph.type": "Chat", "chat_uuid": w.ids["logoMsg"], "chat_from": byMia, "chat_dm": ref("_:dm"),
			"chat_created_at": exported[f.logo], "chat_deleted_at": live, "chat_attachments": []map[string]any{imported(f.logo)}},
		threadPost,
		post("deckPost", byMia, exported[f.deck], imported(f.deck)),
		post("deckForward", byMia, forwardedAt, written(f.deck, byMia, forwardedAt)),
		post("specsPost", byMia, exported[f.specs], imported(f.specs)),
		post("specsForward", byMia, forwardedAt, written(f.specs, byMia, forwardedAt)),
		post("briefPost", byMia, exported[f.brief], imported(f.brief)),
		post("memoPost", byMia, exported[f.memo], imported(f.memo)),
		memoCopy,
	}))
	// Ann's post of 15 Oct naming the brief, written as the server kept a
	// request's file then: with the brief's node's very time, and her as its
	// adder.
	keep(p.dg.Mutate(t, []map[string]any{post("annsBriefPost", byAnn, "2026-10-15T09:00:00Z", written(f.brief, byAnn, exported[f.brief]))}))

	w.exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'finance', true, $2)`, p.channel, mia)
	for _, name := range []string{"pitchPost", "threadPost", "deckPost", "deckForward", "specsPost", "specsForward", "briefPost", "memoPost", "memoCopy"} {
		w.exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, w.ids[name], p.channel, mia)
	}
	w.exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, w.ids["annsBriefPost"], p.channel, p.people["ann"])
	for _, name := range []string{"planReply", "memoReply"} {
		w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, w.ids[name], mia)
	}
	w.exec(`INSERT INTO chats (id, grp_id, created_by) VALUES ($1, $2, $3)`, w.ids["logoMsg"], p.dmMiaAnn, mia)
	return w, f
}

func TestADeletedImportedMessagesFilesOpenForNobody(t *testing.T) {
	w, f := setUpImportedFilesWorld(t)
	id := w.ids

	w.opens("before", w.all...)

	w.deleting("mia", "the imported post", postController.DeletePost, map[string]string{"post_id": id["pitchPost"]})
	w.gone("the imported post deleted", f.pitch)
	w.deleting("mia", "the imported message", chatController.DeleteChat, map[string]string{"chat_id": id["logoMsg"]})
	w.gone("the imported message deleted", f.logo)
	w.deleting("mia", "the imported reply", postController.DeleteCommentInPost, map[string]string{"comment_id": id["planReply"], "post_id": id["threadPost"]})
	w.gone("the imported reply deleted", f.plan)

	// Mia's forward of the deck keeps it after the imported post goes, and
	// takes it when it goes too.
	w.deleting("mia", "the imported deck post", postController.DeletePost, map[string]string{"post_id": id["deckPost"]})
	w.opens("the imported deck post deleted, its forward still there", f.deck)
	w.deleting("mia", "the deck's forward", postController.DeletePost, map[string]string{"post_id": id["deckForward"]})
	w.gone("the deck's forward deleted too", f.deck)

	// The imported specs post still shows its file when Mia's forward goes.
	w.deleting("mia", "the specs' forward", postController.DeletePost, map[string]string{"post_id": id["specsForward"]})
	w.opens("the specs' forward deleted, the imported post still there", f.specs)

	// Ann's post of 15 Oct, with the brief's node's very time, keeps the
	// brief after the imported post goes, and only it does.
	w.deleting("mia", "the imported brief post", postController.DeletePost, map[string]string{"post_id": id["briefPost"]})
	w.opens("the imported brief post deleted, Ann's of 15 Oct still there", f.brief)
	w.deleting("ann", "her post of 15 Oct naming the brief", postController.DeletePost, map[string]string{"post_id": id["annsBriefPost"]})
	w.gone("both posts showing the brief deleted", f.brief)

	// Mia's reply with the memo, sent to the channel too, keeps it while the
	// reply or its copy shows it.
	w.deleting("mia", "the imported memo post", postController.DeletePost, map[string]string{"post_id": id["memoPost"]})
	w.opens("the imported memo post deleted, the reply and its copy still there", f.memo)
	w.deleting("mia", "the reply's copy in the channel", postController.DeletePost, map[string]string{"post_id": id["memoCopy"]})
	w.opens("the copy deleted, the reply still there", f.memo)
	w.deleting("mia", "the reply", postController.DeleteCommentInPost, map[string]string{"comment_id": id["memoReply"], "post_id": id["threadPost"]})
	w.gone("the reply deleted too", f.memo)

	w.stamped(f.specs)
}

// Imported messages deleted before a delete took their files: the repair
// takes them by the same rule, and leaves a file a forward still shows.
func TestARepairTakesTheFilesOfImportedMessagesDeletedBefore(t *testing.T) {
	w, f := setUpImportedFilesWorld(t)
	deletedBefore := func(kind, name string) {
		t.Helper()
		w.dg.Mutate(t, []map[string]any{{"uid": w.graph[name], kind + "_deleted_at": "2026-10-20T10:00:00Z"}})
		w.exec(`UPDATE `+kind+`s SET deleted_at = '2026-10-20T10:00:00Z' WHERE id = $1`, w.ids[name])
	}
	deletedBefore("post", "pitchPost")
	deletedBefore("chat", "logoMsg")
	deletedBefore("comment", "planReply")
	deletedBefore("post", "deckPost") // Mia's forward still shows the deck

	w.opens("before the repair", w.all...)

	rep, did, err := attachmentBusiness.DeletedFilesRepair.Run(context.Background())
	if err != nil || !did {
		t.Fatalf("the repair: ran %v, %v", did, err)
	}
	w.gone("after the repair", f.pitch, f.logo, f.plan)
	w.opens("after the repair", f.deck, f.specs, f.brief, f.memo)
	w.stamped(f.deck, f.specs, f.brief, f.memo)
	if want := (attachmentBusiness.DeletedFilesReport{Posts: 2, Chats: 1, Comments: 1, Files: 3}); rep != want {
		t.Errorf("the repair did %+v, want %+v", rep, want)
	}
}

// A task comment an import kept a file under (business/Import
// attachment_worker: its row's place is the comment), written as the generic
// worker writes it: deleting the comment stamps the file's row, so the purge
// takes it. Every read of it by id already answered 404 once the comment was
// deleted (business/FileAccess importKeptFile asks the comment).
func TestADeletedImportedTaskCommentsFileGoes(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	startMinio(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"deleted":0,"failures":[],"result":"updated","_id":"x","hits":{"total":{"value":0},"hits":[]}}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	var found struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { uid } }`, w.task)), &found); err != nil || len(found.Q) != 1 {
		t.Fatalf("the launch post's node: %+v %v", found, err)
	}
	launch := found.Q[0].UID
	ana := w.people["ana"]

	comment, sheet := uuid.NewString(), uuid.NewString()
	job := uuid.NewString()
	w.exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by) VALUES ($1, 'jira', 'acme', 'api', 'completed', $2)`, job, ana)
	w.exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'comment', $3, $4)`,
		sheet, "userFileUpload/"+ana.String()+"/"+sheet+"_estimates.xlsx", comment, ana)
	w.exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, metadata) VALUES ($1, 'file', 'J-17', $2, $3)`,
		job, sheet, `{"name": "estimates.xlsx", "parent_kind": "comment", "parent_uuid": "`+comment+`"}`)
	w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, comment, ana)
	// The worker's node, and then the copy its comment's update lists: the
	// time of the import's write, the maker by ana's id alone.
	importedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	node := func() map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": sheet, "attachment_file_name": "estimates.xlsx",
			"attachment_obj_key":    "userFileUpload/" + ana.String() + "/" + sheet + "_estimates.xlsx",
			"attachment_created_at": importedAt, "attachment_created_by": map[string]any{"user_uuid": ana.String()}}
	}
	var anaNode struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(user_uuid, %q), first: 1) { uid } }`, ana)), &anaNode); err != nil || len(anaNode.Q) != 1 {
		t.Fatalf("ana's node: %+v %v", anaNode, err)
	}
	w.dg.Mutate(t, []map[string]any{node()}) // the worker's own node first, then its comment's
	w.dg.Mutate(t, []map[string]any{
		{"uid": launch, "task_comments": []map[string]any{{"dgraph.type": "Comment", "comment_uuid": comment,
			"comment_text": "<p>Estimates, from the old tracker.</p>", "comment_task": map[string]any{"uid": launch},
			"comment_by": map[string]any{"uid": anaNode.Q[0].UID}, "comment_created_at": importedAt,
			"comment_deleted_at": "0001-01-01T00:00:00Z", "comment_attachments": []map[string]any{node()}}}},
	})

	call := func(who string, h http.HandlerFunc, params map[string]string, target string, body any) (int, string) {
		t.Helper()
		ctx, _ := w.signedIn(who)
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw)).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
		out := rec.Body.String()
		if len(out) > 200 {
			out = out[:200]
		}
		return rec.Code, out
	}
	fetch := func(who, query string) (int, string) {
		t.Helper()
		return call(who, userController.GetProjectFile, map[string]string{"project_uuid": w.project, "obj_uuid": sheet}, "/"+query, nil)
	}
	for _, who := range []string{"ana", "ben"} {
		if code, body := fetch(who, ""); code != http.StatusOK {
			t.Fatalf("before: %s opening the imported estimates: %d %s", who, code, body)
		}
	}

	if code, body := call("ana", taskController.DeleteTaskComment, nil, "/",
		map[string]string{"task_comment_uuid": comment, "task_uuid": w.task}); code != http.StatusOK {
		t.Fatalf("ana deleting the imported comment: %d %s", code, body)
	}
	for _, who := range []string{"ana", "ben"} {
		for _, query := range []string{"", "?redirect=1"} {
			if code, body := fetch(who, query); code != http.StatusNotFound {
				t.Errorf("%s opened the deleted comment's estimates%s: %d %s", who, query, code, body)
			}
		}
	}
	var stamped bool
	if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM attachments WHERE id = $1`, sheet).Scan(&stamped); err != nil || !stamped {
		t.Errorf("the deleted comment's estimates: row stamped deleted %v (%v), want true: the purge never takes it", stamped, err)
	}
}
