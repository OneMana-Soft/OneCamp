//go:build integration
// +build integration

package integration_test

// The demo's sensitivity labels (miro-44 L; business/DemoSeed labels.go). The
// demo has no admin, so the workspace defines Confidential and Internal as
// itself (labelBusiness.Who{Admin: true}: no person, no agent), and the
// visitor, who owns the launch retro, puts Confidential on it. This makes those
// same calls against a real graph, database and audit chain: the workspace's
// labels are defined and recorded with nobody named, the visitor reads the
// label on the retro, and the visitor's guest link and export are refused in
// the words the web shows.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDemoLabels -v

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	labelBusiness "github.com/akashc777/OneCamp/business/Label"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	labelController "github.com/akashc777/OneCamp/controllers/Label"
)

func TestDemoLabelsAsTheWorkspace(t *testing.T) {
	w := newDocsWorld(t, []string{"visitor", "lee"})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	theWorkspace := labelBusiness.Who{Admin: true}
	define := func(name, colour string, rank int, rules ...string) *labelBusiness.Label {
		t.Helper()
		l, err := labelBusiness.Define(ctx, theWorkspace, labelBusiness.Input{Name: &name, Colour: &colour, Rank: &rank, Rules: &rules})
		if err != nil {
			t.Fatalf("the workspace defines %s as itself: %v", name, err)
		}
		return l
	}
	confidential := define("Confidential", "red", 2, labelBusiness.RuleNoGuestLinks, labelBusiness.RuleNoExport)
	define("Internal", "amber", 1, labelBusiness.RuleNoGuestLinks)
	if confidential.CreatedBy != uuid.Nil {
		t.Errorf("a label the workspace defined names %s as its maker, want no one", confidential.CreatedBy)
	}
	var rows int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'label.defined'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("the workspace's two labels are rows in the audit log: %d (%v)", rows, err)
	}

	retro := w.board(t, "visitor", "Launch retro", false)
	put := w.callAt(labelController.PutItemLabel, "visitor", false, `{"label_id":"`+confidential.ID.String()+`"}`, "kind", boardBusiness.KindBoard, "id", retro)
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), "Confidential") {
		t.Fatalf("the visitor, the retro's owner, labels it Confidential: %d %s", put.Code, put.Body)
	}
	// The body is indented JSON (helpers.WriteJSON), so it is read as JSON, never matched as text.
	rec := w.callAt(labelController.ItemLabel, "visitor", false, "", "kind", boardBusiness.KindBoard, "id", retro)
	var read struct {
		Data struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &read); rec.Code != http.StatusOK || err != nil || read.Data.Name != "Confidential" || read.Data.ID != confidential.ID {
		t.Fatalf("the visitor reads the retro's label: %d %s (%v)", rec.Code, rec.Body, err)
	}
	refused(t, "the visitor makes a guest link to the retro", w.guestLink("visitor", "board", retro), "Confidential")
	refused(t, "the visitor exports the retro", w.call(boardController.RecordBoardExport, "visitor", `{"board_uuid":"`+retro+`","format":"pdf"}`, ""), "Confidential")
	if rec := w.callAt(labelController.DefineLabel, "visitor", false, `{"name":"Mine"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("the visitor, not an admin, defines a label: HTTP %d %s, want 403", rec.Code, rec.Body)
	}
}
