//go:build integration
// +build integration

package integration_test

// A deleted doc is gone, everywhere: opening it, its comments, its files, its
// history and who viewed it; editing it, saving it from the editor and joining
// it live; changing who it's shared with; its share links, new or old; and its
// live updates. Each answers 410 to someone the doc was shared with ("it was
// deleted", so the app can say so) and 404 to anyone else, who learns nothing.
// Its owner still owns it: deleting it again says it's gone, and its links are
// still theirs to see and turn off. (Restoring it brings every surface back:
// TestArchivingAndRestoringADoc.)
//
// Before this, a doc in the trash could still be opened, edited, saved, joined
// live, subscribed to and opened through a guest link, because no doc query
// but one read its deletion time and no rule asked about it.
//
// Editors could also mint links to share a doc outside the workspace, though
// only its owner may share it inside: a link is now its owner's to make, and
// anyone who made one may still find it and turn it off. Listing a doc's links
// refused everyone but its owner, so an editor's link stayed on with no way
// for them to find it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADeletedDocIsGoneEverywhere -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	mqttAccess "github.com/akashc777/OneCamp/business/MqttAccess"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	userController "github.com/akashc777/OneCamp/controllers/User"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADeletedDocIsGoneEverywhere(t *testing.T) {
	t.Setenv("JWT_SECRET", "deleted-doc-integration-secret")
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
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	people := map[string]uuid.UUID{"owner": uuid.New(), "editor": uuid.New(), "viewer": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	doc := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "plans",
		"doc_body": "<p>the plan</p>", "doc_private": true, "doc_public_comment": false,
		"doc_created_at": "2026-10-01T00:00:00Z", "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_by":    map[string]any{"uid": "_:owner"},
		"doc_editing_users": []map[string]any{{"uid": "_:owner"}, {"uid": "_:editor"}},
		"doc_reading_users": []map[string]any{{"uid": "_:viewer"}}})
	uids := dg.Mutate(t, nodes)

	as := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = people[name].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	type call struct {
		who     string            // "" for a request with no signed-in person
		params  map[string]string // the route's {params}
		query   string
		body    any
		headers map[string]string
	}
	do := func(handler http.HandlerFunc, c call) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(c.body)
		req := httptest.NewRequest(http.MethodPost, "/?"+c.query, bytes.NewReader(raw))
		rctx := chi.NewRouteContext()
		for k, v := range c.params {
			rctx.URLParams.Add(k, v)
		}
		reqCtx := context.WithValue(ctx, chi.RouteCtxKey, rctx)
		if c.who != "" {
			reqCtx = context.WithValue(reqCtx, helpers.UserInfoContextKey, as(c.who))
		}
		req = req.WithContext(reqCtx)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	code := func(handler http.HandlerFunc, c call) int { t.Helper(); return do(handler, c).Code }

	// Before it's deleted: a comment, a file, a share link and a guest's way in.
	rec := do(docController.CreateDocComment, call{who: "editor", body: docAdapter.CreateOrUpdateDocCommentInput{DocUuid: doc, CommentBody: "<p>looks good</p>"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("commenting before the delete: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Data docAdapter.OutputCreateCommentForDoc `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	comment := created.Data.Uuid
	file := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'doc', $3, $4)`,
		file, "docs/"+file+".png", doc, people["editor"]); err != nil {
		t.Fatal(err)
	}
	link, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], "doc", doc, "comment", 0, false)
	if err != nil {
		t.Fatalf("a share link before the delete: %v", err)
	}
	grant, err := guestBusiness.ValidateCollabGrant(ctx, link.Token)
	if err != nil {
		t.Fatalf("the share link before the delete: %v", err)
	}
	guestJWT, err := guestBusiness.IssueGuestCollabToken(ctx, grant, "Client")
	if err != nil {
		t.Fatal(err)
	}
	if got := code(docController.GetDocPage, call{who: "viewer", params: map[string]string{"doc_uuid": doc}}); got != http.StatusOK {
		t.Fatalf("the viewer opening the doc before the delete: %d", got)
	}
	if got := code(guestController.GuestDocColabAuthorize, call{body: map[string]string{"doc_uuid": doc},
		headers: map[string]string{"Authorization": "Bearer " + guestJWT}}); got != http.StatusOK {
		t.Fatalf("the guest joining before the delete: %d", got)
	}

	// Who may share it outside the workspace: its owner, as inside it.
	mint := func(who string) int {
		t.Helper()
		return code(guestController.CreateResourceGuestLink, call{who: who, body: map[string]any{"resource_type": "doc", "resource_id": doc, "capability": "view"}})
	}
	if got := mint("editor"); got != http.StatusForbidden {
		t.Errorf("an editor minting a link to share the doc outside: %d, want 403 (only its owner may share it)", got)
	}
	// Someone it was never shared with hears nothing of it: the answer, word
	// for word, to a doc that doesn't exist (it was "only its owner may share
	// it", which said it was there).
	mintFor := func(who, id string) *httptest.ResponseRecorder {
		t.Helper()
		return do(guestController.CreateResourceGuestLink, call{who: who, body: map[string]any{"resource_type": "doc", "resource_id": id, "capability": "view"}})
	}
	missing, stranger := mintFor("stranger", uuid.NewString()), mintFor("stranger", doc)
	if missing.Code != http.StatusNotFound || stranger.Code != missing.Code || stranger.Body.String() != missing.Body.String() {
		t.Errorf("someone it was never shared with minting a link: %d %s, where a doc that doesn't exist answers %d %s",
			stranger.Code, stranger.Body, missing.Code, missing.Body)
	}
	if got := mint("owner"); got != http.StatusOK {
		t.Errorf("its owner minting a link: %d, want 200", got)
	}

	// A link an editor made while editors could still make them: it works
	// until someone turns it off, and its maker may, so they can find it. Its
	// owner sees every link to the doc; anyone else only the ones they made.
	editorLink, err := guestBusiness.CreateResourceGrant(ctx, people["editor"], "doc", doc, "view", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	linksOf := func(who string) []guestController.GuestLinkView {
		t.Helper()
		rec := do(guestController.ListResourceGuestLinks, call{who: who, query: "resource_type=doc&resource_id=" + doc})
		if rec.Code != http.StatusOK {
			t.Errorf("%s listing the doc's links: %d %s, want 200", who, rec.Code, rec.Body)
			return nil
		}
		var got struct {
			Data []guestController.GuestLinkView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Data
	}
	onlyTheirs := func(when string) {
		t.Helper()
		if got := linksOf("owner"); len(got) != 3 {
			t.Errorf("its owner listing the doc's links %s: %d links, want all 3", when, len(got))
		}
		if got := linksOf("editor"); len(got) != 1 || got[0].ID != editorLink.GrantID || !got[0].Mine {
			t.Errorf("an editor listing the doc's links %s: %+v, want only the one they made", when, got)
		}
		if got := linksOf("stranger"); len(got) != 0 {
			t.Errorf("someone it was never shared with listing its links %s: %+v, want none", when, got)
		}
	}
	onlyTheirs("before it was deleted")

	// Its owner deletes it.
	if got := code(docController.DeleteDocTree, call{who: "owner", body: docAdapter.InputUpdateDoc{DocId: doc}}); got != http.StatusOK {
		t.Fatalf("deleting the doc: %d", got)
	}

	title, body := "plans v2", "<p>a new plan</p>"
	gone := []struct {
		name    string
		handler http.HandlerFunc
		c       call
		want    int
	}{
		{"opening it", docController.GetDocPage, call{who: "viewer", params: map[string]string{"doc_uuid": doc}}, http.StatusGone},
		{"opening it, someone it was never shared with", docController.GetDocPage, call{who: "stranger", params: map[string]string{"doc_uuid": doc}}, http.StatusNotFound},
		{"its comments", docController.GetAllCommentList, call{who: "viewer", params: map[string]string{"doc_uuid": doc}}, http.StatusGone},
		{"its comments, someone it was never shared with", docController.GetAllCommentList, call{who: "stranger", params: map[string]string{"doc_uuid": doc}}, http.StatusNotFound},
		{"renaming it", docController.UpdateDocBodyLive, call{who: "editor", body: docAdapter.InputUpdateDoc{DocId: doc, Title: &title}}, http.StatusGone},
		{"rewriting it through its live copy", docController.UpdateDocBodyLive, call{who: "owner", body: docAdapter.InputUpdateDoc{DocId: doc, Body: &body}}, http.StatusGone},
		{"rewriting it through its live copy, someone it was never shared with", docController.UpdateDocBodyLive, call{who: "stranger", body: docAdapter.InputUpdateDoc{DocId: doc, Body: &body}}, http.StatusNotFound},
		{"its versions", docController.ListDocVersions, call{who: "owner", query: "doc_uuid=" + doc}, http.StatusGone},
		{"its versions, someone it was never shared with", docController.ListDocVersions, call{who: "stranger", query: "doc_uuid=" + doc}, http.StatusNotFound},
		{"saving it through the new save route", docController.SaveDocFromCollab, call{body: docBusiness.CollabSave{DocUUID: doc, HTML: body, YjsState: "c3RhdGU="}}, http.StatusGone},
		{"saving only its live state through the new save route", docController.SaveDocFromCollab, call{body: docBusiness.CollabSave{DocUUID: doc, YjsState: "c3RhdGU=", KeepBody: true}}, http.StatusGone},
		{"joining it live", docController.DocCollabAuthorize, call{who: "editor", body: docAdapter.InputUpdateDoc{DocId: doc}}, http.StatusGone},
		{"the collaboration service opening it", docController.GetDocForCollab, call{params: map[string]string{"doc_uuid": doc}}, http.StatusGone},
		{"commenting on it", docController.CreateDocComment, call{who: "editor", body: docAdapter.CreateOrUpdateDocCommentInput{DocUuid: doc, CommentBody: "<p>still here?</p>"}}, http.StatusGone},
		{"editing a comment on it", docController.UpdateDocComment, call{who: "editor", body: docAdapter.CreateOrUpdateDocCommentInput{UUID: comment, DocUuid: doc, CommentBody: "<p>edited</p>"}}, http.StatusGone},
		{"removing a comment on it", docController.DeleteDocComment, call{who: "editor", body: docAdapter.CreateOrUpdateDocCommentInput{UUID: comment, DocUuid: doc}}, http.StatusGone},
		{"reacting to a comment on it", docController.CreateOrUpdateDocCommentReaction, call{who: "viewer", body: docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: uuid.NewString()}}, http.StatusGone},
		{"who it's shared with", docController.GetDocPermissions, call{who: "owner", query: "doc_uuid=" + doc}, http.StatusGone},
		{"sharing it", docController.UpdateDocPermissions, call{who: "owner", body: docAdapter.InputUpdateDocPermissions{DocId: doc, AddViewers: []string{people["stranger"].String()}}}, http.StatusGone},
		{"deleting it again", docController.DeleteDocTree, call{who: "owner", body: docAdapter.InputUpdateDoc{DocId: doc}}, http.StatusGone},
		{"its history", docController.GetDocSnapshots, call{who: "editor", query: "doc_uuid=" + doc}, http.StatusGone},
		{"restoring a version", docController.RestoreDocSnapshot, call{who: "editor", body: docAdapter.InputRestoreDocSnapshot{DocId: doc, SnapshotId: uuid.NewString()}}, http.StatusGone},
		{"recording a view", docController.RecordDocView, call{who: "viewer", body: docAdapter.InputRecordDocView{DocId: doc}}, http.StatusGone},
		{"who viewed it", docController.GetDocViewers, call{who: "owner", query: "doc_uuid=" + doc}, http.StatusGone},
		{"a file in it", userController.GetDocFile, call{who: "viewer", params: map[string]string{"doc_uuid": doc, "obj_uuid": file}}, http.StatusGone},
		{"an image in it", userController.GetDocAttachment, call{who: "viewer", params: map[string]string{"doc_uuid": doc, "obj_uuid": file}}, http.StatusGone},
		{"a new share link", guestController.CreateResourceGuestLink, call{who: "owner", body: map[string]any{"resource_type": "doc", "resource_id": doc, "capability": "view"}}, http.StatusGone},
		{"a guest opening its link", guestController.GuestCollabToken, call{params: map[string]string{"token": link.Token}}, http.StatusNotFound},
		{"a guest joining it live", guestController.GuestDocColabAuthorize, call{body: map[string]string{"doc_uuid": doc}, headers: map[string]string{"Authorization": "Bearer " + guestJWT}}, http.StatusNotFound},
		{"a guest reading its comments", guestController.GuestDocComments, call{params: map[string]string{"token": link.Token}}, http.StatusNotFound},
		{"a guest commenting", guestController.CreateGuestDocComment, call{params: map[string]string{"token": link.Token}, body: map[string]string{"display_name": "Client", "body": "hello?"}}, http.StatusNotFound},
	}
	for _, g := range gone {
		if got := code(g.handler, g.c); got != g.want {
			t.Errorf("%s after the doc was deleted: %d, want %d", g.name, got, g.want)
		}
	}

	// Saving only its live state is one upsert, conditional on the doc being
	// live (domain/Doc WriteCollabSave): one landing after the delete is
	// refused too, not stored on the trashed doc.
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: doc, StateOnly: true, State: "c3RhdGU=", Hash: "hash-of-the-body"}); !errors.Is(err, docDomain.ErrDocDeleted) {
		t.Errorf("the live state's write landing after the delete: %v, want it refused as deleted", err)
	}

	// Its owner may still see and turn off its links, and whoever made one
	// may still find it.
	onlyTheirs("after it was deleted")

	// Nobody's client may subscribe to its live updates.
	if mqttAccess.Allowed(ctx, mqttAccess.Request{Username: "user_" + people["viewer"].String(), Topic: helpers.GetMqttTopicForDoc(doc), Action: "subscribe"}) {
		t.Error("the viewer may still subscribe to the deleted doc's live updates")
	}

	// Nothing changed underneath: still its title and its body, and no live
	// state saved after it was deleted.
	stored := dg.Query(t, fmt.Sprintf(`{ d(func: eq(doc_uuid, %q)) { doc_title doc_body doc_deleted_at doc_yjs_state } }`, doc))
	var parsed struct {
		D []struct {
			Title     string `json:"doc_title"`
			Body      string `json:"doc_body"`
			DeletedAt string `json:"doc_deleted_at"`
			YjsState  string `json:"doc_yjs_state"`
		} `json:"d"`
	}
	if err := json.Unmarshal(stored, &parsed); err != nil || len(parsed.D) != 1 {
		t.Fatalf("reading the doc back: %v %s", err, stored)
	}
	if d := parsed.D[0]; d.Title != "plans" || d.Body != "<p>the plan</p>" || d.YjsState != "" {
		t.Errorf("the deleted doc was changed: title %q, body %q, live state %q", d.Title, d.Body, d.YjsState)
	}

	// Whoever made a link may still turn it off, though only the owner may make one.
	if got := code(guestController.RevokeResourceGuestLink, call{who: "editor", params: map[string]string{"id": editorLink.GrantID.String()}}); got != http.StatusOK {
		t.Errorf("an editor turning off a link they made: %d, want 200", got)
	}
	if got := code(guestController.RevokeResourceGuestLink, call{who: "editor", params: map[string]string{"id": link.GrantID.String()}}); got != http.StatusForbidden {
		t.Errorf("an editor turning off the owner's link: %d, want 403", got)
	}
	if got := linksOf("editor"); len(got) != 0 {
		t.Errorf("an editor listing the doc's links after turning theirs off: %+v, want none", got)
	}
}

// The collaboration service's damaged bodies are repaired in deleted docs too
// (repairdocs), so a doc restored from the trash is whole; the repair leaves
// it deleted. Edits are refused in the trash; this is not an edit.
func TestRepairDocsMendsADeletedDocAndLeavesItDeleted(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
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

	const whole = "<h1>Launch plan</h1><p>Ship on Thursday, after the review with design and legal.</p>"
	collab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"html":%q,"open":false}}`, whole)
	}))
	defer collab.Close()
	t.Setenv("INTERNAL_SECRET", "repair-integration-secret")
	// The render op is on the collaboration service's internal port, which
	// the API finds at COLLAB_LIVE_URL (services/collab).
	t.Setenv("COLLAB_LIVE_URL", collab.URL)

	doc := uuid.NewString()
	deletedAt := "2026-10-01T10:00:00Z"
	dg.Mutate(t, []map[string]any{{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch plan",
		"doc_body": "<p>Error generating HTML</p>", "doc_yjs_state": "c3RhdGU=", "doc_yjs_body_hash": "older",
		"doc_private": false, "doc_deleted_at": deletedAt}})

	var out bytes.Buffer
	rep, err := docBusiness.RepairDocs(ctx, true, &out)
	if err != nil || len(rep.Failed) != 0 || rep.FromState != 1 {
		t.Fatalf("repairing the deleted doc: %s, %v\n%s", rep, err, out.String())
	}
	var got struct {
		D []dgraphStruct.DgraphDoc `json:"d"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ d(func: eq(doc_uuid, %q)) { doc_body doc_deleted_at } }`, doc)), &got); err != nil || len(got.D) != 1 {
		t.Fatalf("read it back: %v", err)
	}
	if got.D[0].Body != whole {
		t.Errorf("its body is %q, want the one rebuilt from its state", got.D[0].Body)
	}
	if !docBusiness.IsDeleted(&got.D[0]) {
		t.Error("repairing a deleted doc brought it back from the trash")
	}
}
