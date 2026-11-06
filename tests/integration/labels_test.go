//go:build integration
// +build integration

package integration_test

// Sensitivity labels (miro-44 head L, business/Label) over the real routes
// against Postgres 12 and Dgraph: an admin names labels; an editor puts one on
// a board, which turns its live link off at once; while it is on, a new link,
// an export told by the browser and a doc's server export are refused with the
// one code and words that say what to do, each refusal a row; an editor can't
// lower it, its owner can; a rule an admin adds holds at once on every item
// the label is on, and a removed label holds nowhere; and the chain verifies.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestSensitivityLabelsOverHTTP' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	labelController "github.com/akashc777/OneCamp/controllers/Label"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// refused says the response is a label's refusal: 403, the one code, and
// words that name the label and what to do.
func refused(t *testing.T, what string, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	var out struct{ Code, Msg string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusForbidden || out.Code != "label_forbids" || !strings.Contains(out.Msg, "labelled "+label+":") ||
		!strings.Contains(out.Msg, "Its owner or a workspace admin can change the label.") {
		t.Fatalf("%s: HTTP %d %s, want 403 label_forbids naming %s", what, rec.Code, rec.Body, label)
	}
}

func TestSensitivityLabelsOverHTTP(t *testing.T) {
	w := newDocsWorld(t, []string{"ada", "maya", "sam", "lee"}, "ada")
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	plan := w.board(t, "maya", "Launch plan", false)
	diary := w.board(t, "maya", "Diary", true)
	sam := w.people["sam"].String()
	if rec := w.call(boardController.UpdateBoardPermissions, "maya", `{"board_uuid":"`+plan+`","add_editors":["`+sam+`"]}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("Maya shares the plan with Sam to edit: %d %s", rec.Code, rec.Body)
	}
	define := func(who string, admin bool, body string) (*httptest.ResponseRecorder, string) {
		rec := w.callAt(labelController.DefineLabel, who, admin, body)
		var out struct {
			Data struct{ ID string } `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out.Data.ID
	}
	put := func(who, kind, id, label string) *httptest.ResponseRecorder {
		body := `{"label_id":null}`
		if label != "" {
			body = `{"label_id":"` + label + `"}`
		}
		return w.callAt(labelController.PutItemLabel, who, false, body, "kind", kind, "id", id)
	}
	exportBoard := func(who string) *httptest.ResponseRecorder {
		return w.call(boardController.RecordBoardExport, who, `{"board_uuid":"`+plan+`","format":"pdf"}`, "")
	}

	var confidential, internal string
	t.Run("only an admin acting themselves defines labels, and names are unique in any case", func(t *testing.T) {
		if rec, _ := define("maya", false, `{"name":"Mine"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("Maya, not an admin, defines a label: HTTP %d %s, want 403", rec.Code, rec.Body)
		}
		var rec *httptest.ResponseRecorder
		if rec, confidential = define("ada", true, `{"name":"Confidential","colour":"red","rank":2,"rules":["no_guest_links","no_export"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("Ada defines Confidential: %d %s", rec.Code, rec.Body)
		}
		if rec, internal = define("ada", true, `{"name":"Internal","colour":"amber","rank":1,"rules":["no_guest_links"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("Ada defines Internal: %d %s", rec.Code, rec.Body)
		}
		if rec, _ := define("ada", true, `{"name":"confidential"}`); rec.Code != http.StatusConflict {
			t.Fatalf("a second Confidential in lower case: HTTP %d %s, want 409", rec.Code, rec.Body)
		}
		if rec, _ := define("ada", true, `{"name":"Secret","rules":["no_printing"]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("a rule nobody registered: HTTP %d %s, want 400", rec.Code, rec.Body)
		}
		if rec := w.callAt(labelController.ListLabels, "lee", false, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Confidential") {
			t.Fatalf("a member lists the labels: %d %s", rec.Code, rec.Body)
		}
	})

	var grant string
	t.Run("an editor's label turns the board's live link off, and both are rows under boards", func(t *testing.T) {
		rec := w.guestLink("maya", "board", plan)
		var out struct {
			Data struct {
				GrantID string `json:"grant_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Data.GrantID == "" {
			t.Fatalf("Maya makes a link before any label: %d %s", rec.Code, rec.Body)
		}
		grant = out.Data.GrantID
		if rec := put("lee", "board", plan, confidential); rec.Code != http.StatusForbidden {
			t.Fatalf("Lee, who can read but not edit, labels the plan: HTTP %d %s, want 403", rec.Code, rec.Body)
		}
		if rec := put("sam", "board", plan, confidential); rec.Code != http.StatusOK {
			t.Fatalf("Sam, an editor, labels the plan Confidential: %d %s", rec.Code, rec.Body)
		}
		var off bool
		if err := w.env.PG.QueryRow(`SELECT revoked_at IS NOT NULL FROM guest_grants WHERE id = $1`, grant).Scan(&off); err != nil || !off {
			t.Fatalf("the plan's live link is still on (%v)", err)
		}
		byAction := map[string]trailRow{}
		for _, r := range trailRows(t, w.env.PG, grant, 3) {
			byAction[r.action] = r
		}
		if r := byAction["board.guest_link_made"]; r.category != "boards" {
			t.Errorf("the link's own row: %+v", byAction)
		}
		if r := byAction["board.label_applied"]; r.category != "boards" || r.actor != sam ||
			r.summary != "Turned off 1 external link: the board is labelled “Confidential”" {
			t.Errorf("the turn-off's row: %+v", byAction)
		}
		if rec := put("sam", "board", plan, confidential); rec.Code != http.StatusOK {
			t.Fatalf("the same label again: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("while it is on, a link and an export are refused, each a row saying who, what and which rule", func(t *testing.T) {
		refused(t, "Maya makes a link to the labelled plan", w.guestLink("maya", "board", plan), "Confidential")
		refused(t, "Sam exports the labelled plan", exportBoard("sam"), "Confidential")
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'board.label_refused' AND category = 'boards'
			AND metadata->>'item_id' = $1 AND metadata->>'rule' IN ('no_guest_links', 'no_export')`, plan).Scan(&n); err != nil || n != 2 {
			t.Fatalf("refusal rows: %d (%v), want 2", n, err)
		}
		var labelled int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'board.labelled' AND metadata->>'item_id' = $1`, plan).Scan(&labelled); err != nil || labelled != 1 {
			t.Fatalf("labelled rows: %d (%v), want 1 (putting the same label again writes nothing)", labelled, err)
		}
	})

	t.Run("a refusal while an agent acts for a person names the agent and its token beside them", func(t *testing.T) {
		agent, token := uuid.NewString(), uuid.NewString()
		// The context the API token middleware leaves for a token bound to an agent.
		ctx := context.WithValue(context.WithValue(w.as("sam", ""), helpers.ApiTokenIDContextKey, token), helpers.ApiAgentIDContextKey, agent)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"board_uuid":"`+plan+`","format":"pdf"}`)).WithContext(ctx)
		rec := httptest.NewRecorder()
		boardController.RecordBoardExport(rec, req)
		refused(t, "Sam's agent exports the labelled plan through his token", rec, "Confidential")
		var kind, actor, gotAgent, gotToken string
		if err := w.env.PG.QueryRow(`SELECT COALESCE(actor_kind, ''), COALESCE(actor_id::text, ''), COALESCE(metadata->>'agent_uuid', ''), COALESCE(metadata->>'token_id', '')
			FROM admin_audit_log WHERE action = 'board.label_refused' AND metadata->>'item_id' = $1 AND metadata->>'rule' = 'no_export'
			ORDER BY seq DESC LIMIT 1`, plan).Scan(&kind, &actor, &gotAgent, &gotToken); err != nil {
			t.Fatal(err)
		}
		if kind != "agent" || actor != w.people["sam"].String() || gotAgent != agent || gotToken != token {
			t.Fatalf("the refusal row names %q %s, agent %q, token %q; want agent %s acting for Sam (%s) through token %s",
				kind, actor, gotAgent, gotToken, agent, w.people["sam"], token)
		}
	})

	t.Run("an editor can't lower or take off the label; its owner can", func(t *testing.T) {
		for what, label := range map[string]string{"lowers it to Internal": internal, "takes it off": ""} {
			rec := put("sam", "board", plan, label)
			var out struct{ Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != http.StatusForbidden || out.Code != "label_owner_lowers" {
				t.Fatalf("Sam %s: HTTP %d %s, want 403 label_owner_lowers", what, rec.Code, rec.Body)
			}
		}
		if rec := put("maya", "board", plan, internal); rec.Code != http.StatusOK {
			t.Fatalf("Maya, its owner, lowers it to Internal: %d %s", rec.Code, rec.Body)
		}
		if rec := exportBoard("sam"); rec.Code != http.StatusOK {
			t.Fatalf("an export under Internal (no rule against it): %d %s", rec.Code, rec.Body)
		}
		refused(t, "a link under Internal", w.guestLink("maya", "board", plan), "Internal")
	})

	t.Run("a rule an admin adds holds at once; a removed label holds nowhere", func(t *testing.T) {
		if rec := w.callAt(labelController.ChangeLabel, "maya", false, `{"rules":[]}`, "id", internal); rec.Code != http.StatusForbidden {
			t.Fatalf("Maya changes a label: HTTP %d %s, want 403", rec.Code, rec.Body)
		}
		if rec := w.callAt(labelController.ChangeLabel, "ada", true, `{"rules":["no_guest_links","no_export"]}`, "id", internal); rec.Code != http.StatusOK {
			t.Fatalf("Ada gives Internal no_export: %d %s", rec.Code, rec.Body)
		}
		refused(t, "an export once Internal forbids it", exportBoard("sam"), "Internal")
		if rec := w.callAt(labelController.RemoveLabel, "ada", true, "", "id", internal); rec.Code != http.StatusOK {
			t.Fatalf("Ada removes Internal: %d %s", rec.Code, rec.Body)
		}
		if rec := exportBoard("sam"); rec.Code != http.StatusOK {
			t.Fatalf("an export once Internal is removed: %d %s", rec.Code, rec.Body)
		}
		if rec := w.guestLink("maya", "board", plan); rec.Code != http.StatusOK {
			t.Fatalf("a link once Internal is removed: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("a doc's label refuses its export, told or made on the server", func(t *testing.T) {
		notes := w.doc(t, "maya", "Board notes", false)
		if rec := put("maya", "doc", notes, confidential); rec.Code != http.StatusOK {
			t.Fatalf("Maya labels her doc: %d %s", rec.Code, rec.Body)
		}
		body := `{"doc_uuid":"` + notes + `","format":"md"}`
		refused(t, "the browser's export of the doc", w.callAt(contentIOController.RecordExport, "maya", false, body), "Confidential")
		refused(t, "the server's export of the doc", w.callAt(contentIOController.ExportDoc, "maya", false, body), "Confidential")
		refused(t, "a link to the doc", w.guestLink("maya", "doc", notes), "Confidential")
		if rec := w.callAt(labelController.ItemLabel, "maya", false, "", "kind", "doc", "id", notes); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Confidential") {
			t.Fatalf("the doc's label for its owner: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("a label is read only by someone who may read the item, and never names a private one", func(t *testing.T) {
		if rec := put("maya", "board", diary, confidential); rec.Code != http.StatusOK {
			t.Fatalf("Maya labels her diary: %d %s", rec.Code, rec.Body)
		}
		if rec := w.callAt(labelController.ItemLabel, "lee", false, "", "kind", "board", "id", diary); rec.Code != http.StatusNotFound {
			t.Fatalf("Lee reads the label of Maya's private diary: HTTP %d %s, want 404", rec.Code, rec.Body)
		}
		// A list's chips: Maya sees both, Lee only the plan's (none now) and never the diary's.
		list := func(who string) map[string]json.RawMessage {
			info := userModels.UserInfo{}
			info.UserPostgresInfo.Id = w.people[who]
			info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[who], Uuid: w.people[who].String()}
			route := chi.NewRouteContext()
			route.URLParams.Add("kind", "board")
			ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, info), chi.RouteCtxKey, route)
			req := httptest.NewRequest(http.MethodGet, "/labels/items/board?ids="+plan+","+diary, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			labelController.ItemLabels(rec, req)
			var out struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil {
				t.Fatalf("%s's list read: %d %s", who, rec.Code, rec.Body)
			}
			return out.Data
		}
		if got := list("maya"); len(got) != 1 || got[diary] == nil {
			t.Fatalf("Maya's chips: %v, want the diary's only (Internal, the plan's, was removed)", got)
		}
		if got := list("lee"); len(got) != 0 {
			t.Fatalf("Lee's chips: %v, want none (the diary is private to Maya)", got)
		}
		var row struct{ Summary string }
		if err := w.env.PG.QueryRow(`SELECT summary FROM admin_audit_log WHERE action = 'board.labelled' AND metadata->>'item_id' = $1`, diary).Scan(&row.Summary); err != nil ||
			row.Summary != "Labelled a private board “Confidential”" {
			t.Fatalf("the diary's row: %q (%v)", row.Summary, err)
		}
	})
	time.Sleep(200 * time.Millisecond)
	chainVerifies(t)
}
