//go:build integration
// +build integration

package integration_test

// Relation cells linking to OneCamp items, read by people who may open
// different things, against Postgres 12 and Dgraph (TAB-BUG-5).
//
// A link to a doc, task, board, project or person was stored with a label
// copied when it was made, and every reader of the table saw that label, and
// so did anyone with its guest link: Maya's private doc's title, to the whole
// workspace. Now each read names the items for its reader by each kind's own
// rule, and a write keeps a link's id and type alone. The rows here are
// seeded as installs hold them: with labels copied, one of them forged.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableItemTitles -v

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/business/DataTable/formula"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTableItemTitles(t *testing.T) {
	w := newTableWorld(t, "maya", "bo")
	dg := integration.SetupDgraph(t)
	maya, bo := w.people["maya"], w.people["bo"]
	const live = "0001-01-01T00:00:00Z"
	plan, handbook, layoffs, move, closeTask := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.UserID.String(), "user_name": "maya", "user_full_name": "Maya Iyer"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo.UserID.String(), "user_name": "bo", "user_full_name": "Bo Chen"},
		// Maya's private doc and board; a doc for everyone.
		{"uid": "_:plan", "dgraph.type": "Doc", "doc_uuid": plan, "doc_title": "Restructuring plan Q4", "doc_private": true,
			"doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:maya"}},
		{"uid": "_:handbook", "dgraph.type": "Doc", "doc_uuid": handbook, "doc_title": "Handbook", "doc_private": false,
			"doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:maya"}},
		{"uid": "_:layoffs", "dgraph.type": "Board", "board_uuid": layoffs, "board_title": "Layoffs", "board_private": true,
			"board_deleted_at": live, "board_created_by": map[string]any{"uid": "_:maya"}},
		// A project Maya is in and Bo isn't, and a task in it.
		{"uid": "_:move", "dgraph.type": "Project", "project_uuid": move, "project_name": "Pune move", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:maya"}}},
		{"uid": "_:close", "dgraph.type": "Task", "task_uuid": closeTask, "task_name": "Close the Pune office", "task_deleted_at": live,
			"task_project": map[string]any{"uid": "_:move"}, "task_created_by": map[string]any{"uid": "_:maya"}},
	})
	secrets := []string{"Restructuring plan Q4", "Layoffs", "Pune move", "Close the Pune office", "FORGED"}
	// Maya is named as the app names people everywhere.
	people, err := userDomain.ResolveUserDisplays(w.ctx, []string{maya.UserID.String()})
	if err != nil || people[maya.UserID.String()].Display == "" {
		t.Fatalf("Maya has no name to show: %v", err)
	}
	mayaName := people[maya.UserID.String()].Display

	tb := w.table("maya", "Plans")
	linked := w.field("maya", tb, "Linked", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "any"})
	titles := w.field("maya", tb, "Titles", dataTableModel.FieldFormula, map[string]interface{}{"formula": "{Linked}"})
	stored := fmt.Sprintf(`[{"id":%q,"label":"Restructuring plan Q4","type":"doc"},{"id":%q,"label":"FORGED","type":"doc"},`+
		`{"id":%q,"label":"Layoffs","type":"board"},{"id":%q,"label":"Pune move","type":"project"},`+
		`{"id":%q,"label":"Close the Pune office","type":"task"},{"id":%q,"label":"FORGED","type":"user"}]`,
		plan, handbook, layoffs, move, closeTask, maya.UserID)
	var row uuid.UUID
	if err := w.env.PG.QueryRow(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		VALUES (uuid_generate_v4(), $1, jsonb_build_object($2::text, $3::jsonb), 1, $4) RETURNING id`,
		tb.Id, linked, stored, maya.UserID).Scan(&row); err != nil {
		t.Fatalf("seed the row: %v", err)
	}

	// read is the row's links and formula value in a bundle, and the bundle's
	// whole text.
	type link struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Label  string `json:"label"`
		Hidden bool   `json:"hidden"`
	}
	read := func(t *testing.T, b *dataTableBusiness.TableBundle) (map[string]link, string, string) {
		t.Helper()
		whole, _ := json.Marshal(b)
		for _, r := range b.Rows {
			if r.Id != row {
				continue
			}
			var cells map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.Values), &cells); err != nil {
				t.Fatal(err)
			}
			var links []link
			_ = json.Unmarshal(cells[linked], &links)
			byID := map[string]link{}
			for _, l := range links {
				byID[l.ID] = l
			}
			var formula string
			_ = json.Unmarshal(cells[titles], &formula)
			return byID, formula, string(whole)
		}
		t.Fatalf("the row isn't in the bundle")
		return nil, "", ""
	}
	bundleFor := func(t *testing.T, who dataTableBusiness.Actor) *dataTableBusiness.TableBundle {
		t.Helper()
		b, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, who)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	noSecrets := func(t *testing.T, who, text string, but ...string) {
		t.Helper()
		for _, s := range secrets {
			allowed := false
			for _, b := range but {
				allowed = allowed || b == s
			}
			if !allowed && strings.Contains(text, s) {
				t.Errorf("%s's read has %q", who, s)
			}
		}
	}

	t.Run("Bo gets the titles of what he may open, and nothing else", func(t *testing.T) {
		links, formula, whole := read(t, bundleFor(t, bo))
		noSecrets(t, "Bo", whole)
		for id, what := range map[string]string{plan: "the private doc", layoffs: "the private board", move: "the project", closeTask: "its task"} {
			if l := links[id]; !l.Hidden || l.Label != "" {
				t.Errorf("Bo reads %s as %+v, want it hidden", what, l)
			}
		}
		if links[handbook].Label != "Handbook" || links[maya.UserID.String()].Label != mayaName {
			t.Errorf("Bo reads the handbook as %+v and Maya as %+v", links[handbook], links[maya.UserID.String()])
		}
		if !strings.HasPrefix(formula, "Handbook, ") || strings.Count(formula, ", ") != 1 {
			t.Errorf("Bo's formula reads %q", formula)
		}
	})

	t.Run("Maya gets every current title, and never a stored label", func(t *testing.T) {
		links, _, whole := read(t, bundleFor(t, maya))
		noSecrets(t, "Maya", whole, "Restructuring plan Q4", "Layoffs", "Pune move", "Close the Pune office")
		for id, want := range map[string]string{plan: "Restructuring plan Q4", handbook: "Handbook", layoffs: "Layoffs", move: "Pune move", closeTask: "Close the Pune office"} {
			if links[id].Label != want {
				t.Errorf("Maya reads %s as %+v, want %q", id, links[id], want)
			}
		}
	})

	t.Run("an agent of Maya's asked by Bo shows only what both may open", func(t *testing.T) {
		b, err := dataTableBusiness.GetBundle(dataTableBusiness.AlsoFor(w.ctx, bo), tb.Id, maya)
		if err != nil {
			t.Fatal(err)
		}
		_, _, whole := read(t, b)
		noSecrets(t, "Maya's agent asked by Bo", whole)
	})

	t.Run("a guest link shows people's names and no other item", func(t *testing.T) {
		b, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		links, _, whole := read(t, b)
		noSecrets(t, "a guest", whole)
		for _, id := range []string{plan, handbook, layoffs, move, closeTask} {
			if strings.Contains(whole, id) {
				t.Errorf("the guest bundle names item %s", id)
			}
		}
		// Without Maya's id: a person's id is a member's, which no guest sees.
		if len(links) != 1 || links[""].Label != mayaName || strings.Contains(whole, maya.UserID.String()) {
			t.Errorf("a guest reads %+v", links)
		}
	})

	t.Run("totals and the rows endpoint name items for their reader too", func(t *testing.T) {
		res, _, err := dataTableBusiness.AggregateTable(w.ctx, tb.Id, bo, dataTableBusiness.QuerySpec{Op: dataTableBusiness.OpCount, GroupBy: "Linked"})
		if err != nil {
			t.Fatal(err)
		}
		text, _ := json.Marshal(res)
		noSecrets(t, "Bo's totals", string(text))
		if strings.Contains(string(text), plan) {
			t.Errorf("Bo's totals group by the private doc's id: %s", text)
		}
		rows, err := dataTableBusiness.ListRows(w.ctx, tb.Id, bo, 10, 0)
		if err != nil || len(rows) != 1 {
			t.Fatal(err)
		}
		noSecrets(t, "Bo's rows", rows[0].Values)
	})

	t.Run("a renamed doc reads by its new title, a deleted task as hidden", func(t *testing.T) {
		dg.Mutate(t, []map[string]any{
			{"uid": uids["handbook"], "doc_title": "Handbook 2026"},
			{"uid": uids["close"], "task_deleted_at": time.Now().UTC().Format(time.RFC3339)},
		})
		bos, _, _ := read(t, bundleFor(t, bo))
		mayas, _, _ := read(t, bundleFor(t, maya))
		if bos[handbook].Label != "Handbook 2026" {
			t.Errorf("after the rename Bo reads %+v", bos[handbook])
		}
		if !mayas[closeTask].Hidden {
			t.Errorf("after the task's deletion Maya reads %+v", mayas[closeTask])
		}
	})

	// The update refused it while the dry run said it would change nothing,
	// so the menu saved it at once, and failed.
	t.Run("the dry run of a link becoming text refuses it, as the update does", func(t *testing.T) {
		_, previewErr := dataTableBusiness.PreviewConversion(w.ctx, tb.Id, uuid.MustParse(linked), dataTableModel.FieldText, formula.NumberSigns{}, maya)
		_, updateErr := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(linked), dataTableBusiness.FieldPatch{Name: ptr("Linked"), Type: ptr(dataTableModel.FieldText)}, maya)
		if previewErr == nil || updateErr == nil || previewErr.Error() != updateErr.Error() {
			t.Errorf("the dry run answered %v and the update %v; want the same refusal", previewErr, updateErr)
		}
	})

	t.Run("a write keeps each link's id and type alone, and refuses what isn't a link", func(t *testing.T) {
		_, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row, map[string]interface{}{linked: []interface{}{
			map[string]interface{}{"id": plan, "type": "doc", "label": "FORGED", "hidden": true},
			map[string]interface{}{"id": handbook, "type": "doc"},
		}}, bo)
		if err != nil {
			t.Fatal(err)
		}
		var cell string
		if err := w.env.PG.QueryRow(`SELECT values -> $2 FROM data_table_rows WHERE id = $1`, row, linked).Scan(&cell); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`[{"id": %q, "type": "doc"}, {"id": %q, "type": "doc"}]`, plan, handbook); cell != want {
			t.Errorf("the cell is stored as %s, want %s", cell, want)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row, map[string]interface{}{linked: []interface{}{
			map[string]interface{}{"label": "Salaries", "type": "doc"},
		}}, bo); err == nil {
			t.Error("a link with no id was stored")
		}
	})
}
