package business

import (
	"context"
	"errors"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	boardModels "github.com/akashc777/OneCamp/models/dgraph/Board"
)

type emitted []resourceEventBusiness.Event

func (e *emitted) Emit(_ context.Context, ev resourceEventBusiness.Event) { *e = append(*e, ev) }

// A change that gives people a place on a board is told once per level
// given, after it is made; lowering or taking a place is recorded before the
// change instead (RecordedTaking), so it is never told here.
func TestBoardSharesAreToldOncePerLevelGiven(t *testing.T) {
	var got emitted
	old := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = &got
	t.Cleanup(func() { resourceEventBusiness.Bus = old })
	private := true
	board := &dgraphStruct.DgraphBoard{Uuid: "b1", Title: "Launch plan", IsPrivate: &private}
	shifts := []auditBusiness.AccessShift{{UserUUID: "u1", To: "edit"}, {UserUUID: "u2", To: "view"}, {UserUUID: "u3", From: "view", To: "edit"}, {UserUUID: "u4", From: "edit", To: "view"}}
	emitBoardShared(context.Background(), board, shifts)
	if len(got) != 2 {
		t.Fatalf("events %+v, want one per level given", got)
	}
	edit, view := got[0], got[1]
	if edit.Kind != resourceEventBusiness.BoardShared || edit.ResourceKind != KindBoard || edit.ResourceUUID != "b1" ||
		edit.Data["level"] != "edit" || len(edit.Subjects) != 2 || edit.Subjects[0] != "u1" || edit.Subjects[1] != "u3" ||
		edit.Data["private"] != "true" || edit.Data["title"] != "Launch plan" {
		t.Errorf("edit share %+v", edit)
	}
	if view.Data["level"] != "view" || len(view.Subjects) != 1 || view.Subjects[0] != "u2" {
		t.Errorf("view share %+v", view)
	}
	got = nil
	emitBoardShared(context.Background(), board, shifts[3:])
	if len(got) != 0 {
		t.Errorf("a lowering alone was told as a share: %+v", got)
	}
}

// An export is told only for a board the person can read, in a format the
// canvas makes; anything else answers not found (or a bad format) and tells
// nothing, so the log never claims an export of a board someone can't open. A
// uuid naming no board gets the read's own answer, its NotFound (the stub
// answers as models/dgraph/Board does), and a read that failed stays a
// failure, never a not found.
func TestRecordBoardExportTellsOnlyAReadableBoardsExport(t *testing.T) {
	var got emitted
	oldBus, oldRead := resourceEventBusiness.Bus, exportBoardRead
	resourceEventBusiness.Bus = &got
	t.Cleanup(func() { resourceEventBusiness.Bus, exportBoardRead = oldBus, oldRead })
	private, public := true, false
	boards := map[string]*dgraphStruct.DgraphBoard{
		"open":   {Uuid: "open", Title: "Roadmap", IsPrivate: &public},
		"closed": {Uuid: "closed", Title: "Secret", IsPrivate: &private, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
	}
	readFailed := errors.New("dgraph didn't answer")
	exportBoardRead = func(_ context.Context, id, _ string) (*dgraphStruct.DgraphBoard, error) {
		switch {
		case id == "broken":
			return nil, readFailed
		case boards[id] == nil:
			return nil, boardModels.ErrNotFound
		}
		return boards[id], nil
	}
	ctx := context.Background()
	if err := RecordBoardExport(ctx, "open", "exe", "uid", "reader"); err != ErrBoardExportFormat {
		t.Errorf("an unknown format: %v", err)
	}
	for _, id := range []string{"closed", "missing"} {
		if err := RecordBoardExport(ctx, id, "pdf", "uid", "reader"); err != ErrBoardNotFound || !errors.Is(err, helpers.ErrNotFound) {
			t.Errorf("%s: %v, want not found", id, err)
		}
	}
	if err := RecordBoardExport(ctx, "broken", "pdf", "uid", "reader"); !errors.Is(err, readFailed) || errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("a failed read: %v, want the failure itself", err)
	}
	if len(got) != 0 {
		t.Fatalf("told %+v for boards the person can't read", got)
	}
	if err := RecordBoardExport(ctx, "open", "picture", "uid", "reader"); err != nil {
		t.Fatalf("a readable board: %v", err)
	}
	if len(got) != 1 || got[0].Kind != resourceEventBusiness.BoardExported || got[0].ResourceUUID != "open" ||
		got[0].Data["format"] != "picture" || got[0].Data["via"] != "browser" || got[0].Data["private"] != "false" {
		t.Fatalf("told %+v, want one board.exported of the picture, in the browser", got)
	}
}
