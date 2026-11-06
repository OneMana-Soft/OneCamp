package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/business/Board/scene"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	samUUID = "11111111-1111-4111-8111-111111111111"
	edUUID  = "22222222-2222-4222-8222-222222222222"
	adaUUID = "33333333-3333-4333-8333-333333333333"
	brdUUID = "44444444-4444-4444-8444-444444444444"
)

func protectBoard(private bool, edits uint8) *dgraphStruct.DgraphBoard {
	return &dgraphStruct.DgraphBoard{Uuid: brdUUID, Title: "Launch plan", IsPrivate: &private, HasEditAccess: edits,
		CreatedBy: &dgraphStruct.DgraphUser{Uuid: samUUID}}
}

func TestCanProtectIsTheOwnerOrAnAdmin(t *testing.T) {
	gone := protectBoard(false, 1)
	at := time.Now()
	gone.DeletedAt = &at
	for _, c := range []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		user  string
		admin bool
		want  bool
	}{
		{"its owner", protectBoard(false, 0), samUUID, false, true},
		{"a workspace admin", protectBoard(false, 0), adaUUID, true, true},
		{"an editor", protectBoard(false, 1), edUUID, false, false},
		{"a reader", protectBoard(false, 0), edUUID, false, false},
		{"its owner, once it is deleted", gone, samUUID, false, false},
		{"an admin, once it is deleted", gone, adaUUID, true, false},
		{"no board", nil, samUUID, true, false},
	} {
		if got := CanProtect(c.board, c.user, c.admin); got != c.want {
			t.Errorf("%s: CanProtect = %v, want %v", c.name, got, c.want)
		}
	}
}

// protectWorld swaps every seam ProtectElements goes through.
type protectWorld struct {
	board    *dgraphStruct.DgraphBoard
	readErr  error
	admins   map[string]bool
	live     []string
	liveErr  error
	changed  int
	rows     []string
	metas    []map[string]interface{}
	auditErr error
}

func (w *protectWorld) swap(t *testing.T) {
	t.Helper()
	read, admin, live, recorded, refused := protectSeams.read, protectSeams.isAdmin, protectSeams.live, protectSeams.recorded, protectSeams.refused
	t.Cleanup(func() {
		protectSeams.read, protectSeams.isAdmin, protectSeams.live, protectSeams.recorded, protectSeams.refused = read, admin, live, recorded, refused
	})
	protectSeams.read = func(_ context.Context, _, _ string) (*dgraphStruct.DgraphBoard, error) { return w.board, w.readErr }
	protectSeams.isAdmin = func(_ context.Context, user string) bool { return w.admins[user] }
	protectSeams.live = func(_ context.Context, board string, ids []string, on bool, by, name string) (int, error) {
		w.live = append(w.live, fmt.Sprintf("%s %v %v %s %s", board, ids, on, by, name))
		return w.changed, w.liveErr
	}
	protectSeams.recorded = func(_ context.Context, _ auditBusiness.Principal, action, category, summary string, meta map[string]interface{}, change func() error) error {
		if category != auditBusiness.CategoryBoards {
			t.Errorf("%s is filed under %q, not boards", action, category)
		}
		if w.auditErr != nil {
			return fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, w.auditErr)
		}
		w.rows = append(w.rows, action+": "+summary)
		w.metas = append(w.metas, meta)
		if err := change(); err != nil {
			w.rows = append(w.rows, action+"_failed")
			return err
		}
		return nil
	}
	protectSeams.refused = func(_ context.Context, _ auditBusiness.Principal, action, category, summary string, meta map[string]interface{}) error {
		w.rows = append(w.rows, action+": "+summary)
		w.metas = append(w.metas, meta)
		return nil
	}
}

func TestProtectingIsTheOwnersOrAnAdminsAndEveryLockAndLiftIsInTheLog(t *testing.T) {
	ctx := context.Background()
	w := &protectWorld{board: protectBoard(false, 1), admins: map[string]bool{adaUUID: true}, changed: 2}
	w.swap(t)

	n, err := ProtectElements(ctx, brdUUID, []string{"e-1", "e-2", "e-1"}, true, "0x1", samUUID, "Sam")
	if err != nil || n != 2 {
		t.Fatalf("the owner protecting: %d, %v", n, err)
	}
	if want := brdUUID + " [e-1 e-2] true " + samUUID + " Sam"; len(w.live) != 1 || w.live[0] != want {
		t.Fatalf("the live op: %q, want %q (a named id once)", w.live, want)
	}
	if want := "board.protect.locked: Protected 2 objects on ‘Launch plan’"; len(w.rows) != 1 || w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
	meta := w.metas[0]
	if meta["resource_uuid"] != brdUUID || meta["resource_kind"] != KindBoard || meta["count"] != 2 || meta["element_ids"] != "e-1,e-2" || meta["title"] != "Launch plan" {
		t.Fatalf("the row's metadata: %v", meta)
	}

	w.live, w.rows, w.metas, w.changed = nil, nil, nil, 1
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, false, "0x3", adaUUID, "Ada"); err != nil {
		t.Fatalf("an admin lifting: %v", err)
	}
	if want := "board.protect.lifted: Lifted protection from 1 object on ‘Launch plan’"; len(w.rows) != 1 || w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
}

func TestAnEditorIsRefusedAndTheRefusalIsInTheLog(t *testing.T) {
	w := &protectWorld{board: protectBoard(false, 1), changed: 1}
	w.swap(t)
	_, err := ProtectElements(context.Background(), brdUUID, []string{"e-1"}, false, "0x2", edUUID, "Ed")
	if !errors.Is(err, ErrCannotProtect) {
		t.Fatalf("an editor lifting: %v, want ErrCannotProtect", err)
	}
	if len(w.live) != 0 {
		t.Fatalf("the live op ran for an editor: %q", w.live)
	}
	if want := "board.protect_refused: Refused lifting protection from 1 object on ‘Launch plan’"; len(w.rows) != 1 || w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
}

func TestAPrivateBoardIsNeverNamedAndOneUnreadIsNotThere(t *testing.T) {
	ctx := context.Background()
	w := &protectWorld{board: protectBoard(true, 0), changed: 1}
	w.swap(t)
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, true, "0x1", samUUID, "Sam"); err != nil {
		t.Fatalf("the owner protecting on their private board: %v", err)
	}
	if want := "board.protect.locked: Protected 1 object on a private board"; w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
	if _, named := w.metas[0]["title"]; named {
		t.Fatalf("a private board's row carries its title: %v", w.metas[0])
	}

	w.live, w.rows = nil, nil
	w.admins = map[string]bool{adaUUID: true}
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, false, "0x3", adaUUID, "Ada"); !errors.Is(err, ErrBoardNotFound) {
		t.Fatalf("an admin a private board isn't shared with: %v, want ErrBoardNotFound", err)
	}
	w.readErr = helpers.NotFound("no board")
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, true, "0x1", samUUID, "Sam"); !errors.Is(err, ErrBoardNotFound) {
		t.Fatalf("no board: %v, want ErrBoardNotFound", err)
	}
	if len(w.live) != 0 || len(w.rows) != 0 {
		t.Fatalf("a board that isn't there was changed or logged: %q %q", w.live, w.rows)
	}
}

func TestProtectRefusesAListItCannotTake(t *testing.T) {
	w := &protectWorld{board: protectBoard(false, 0), changed: 1}
	w.swap(t)
	many := make([]string, MaxProtect+1)
	for i := range many {
		many[i] = fmt.Sprintf("e-%d", i)
	}
	for name, ids := range map[string][]string{
		"none":        nil,
		"an empty id": {"e-1", " "},
		"over 500":    many,
		"a long id":   {strings.Repeat("x", 65)},
	} {
		if _, err := ProtectElements(context.Background(), brdUUID, ids, true, "0x1", samUUID, "Sam"); !errors.Is(err, ErrProtectIDs) {
			t.Errorf("%s: %v, want ErrProtectIDs", name, err)
		}
	}
	if len(w.live) != 0 || len(w.rows) != 0 {
		t.Fatalf("a refused list was changed or logged: %q %q", w.live, w.rows)
	}
}

func TestNoRecordNoLockAndAFailedLockIsLogged(t *testing.T) {
	ctx := context.Background()
	w := &protectWorld{board: protectBoard(false, 0), changed: 1, auditErr: errors.New("chain down")}
	w.swap(t)
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, true, "0x1", samUUID, "Sam"); !errors.Is(err, auditBusiness.ErrNotRecorded) {
		t.Fatalf("an unwritten row: %v, want ErrNotRecorded", err)
	}
	if len(w.live) != 0 {
		t.Fatalf("protected with no record: %q", w.live)
	}
	w.auditErr, w.liveErr = nil, errors.New("collab down")
	if _, err := ProtectElements(ctx, brdUUID, []string{"e-1"}, true, "0x1", samUUID, "Sam"); err == nil || errors.Is(err, ErrCannotProtect) {
		t.Fatalf("a live op that failed: %v", err)
	}
	if len(w.rows) != 2 || w.rows[1] != "board.protect.locked_failed" {
		t.Fatalf("the log of a lock that didn't take: %q", w.rows)
	}
}

// protectedState is a board state with the elements and the protected record
// given.
func protectedState(t *testing.T, protected ...string) string {
	t.Helper()
	els := []scene.Element{{"id": "e-1", "type": "rectangle", "version": 1}, {"id": "e-2", "type": "rectangle", "version": 1}}
	rec := scene.Root{Name: "protected"}
	for _, id := range protected {
		rec.Entries = append(rec.Entries, scene.Entry{Key: id, Value: map[string]any{"by": samUUID, "name": "Sam", "at": 1}})
	}
	state, _, err := scene.State(7, els, nil, nil, rec)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestARestoreSaysWhatProtectionItLiftsAndBringsBack(t *testing.T) {
	for _, c := range []struct {
		name          string
		now, version  []string
		lifted, again string
	}{
		{"nothing protected", nil, nil, "", ""},
		{"the same protected", []string{"e-1"}, []string{"e-1"}, "", ""},
		{"a version from before the lock", []string{"e-1"}, nil, "e-1", ""},
		{"a version from while it was locked", nil, []string{"e-2", "e-1"}, "", "e-1,e-2"},
		{"both", []string{"e-1"}, []string{"e-2"}, "e-1", "e-2"},
	} {
		lifted, again, err := restoreProtection(protectedState(t, c.now...), protectedState(t, c.version...))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Join(lifted, ",") != c.lifted || strings.Join(again, ",") != c.again {
			t.Errorf("%s: lifted %q again %q, want %q %q", c.name, lifted, again, c.lifted, c.again)
		}
	}
	if _, _, err := restoreProtection("not base64!", protectedState(t)); err == nil {
		t.Fatal("a state that can't be read restores as if nothing were protected")
	}
	if lifted, again, err := restoreProtection("", protectedState(t, "e-1")); err != nil || len(lifted) != 0 || strings.Join(again, ",") != "e-1" {
		t.Fatalf("a board never drawn on: %q %q %v", lifted, again, err)
	}
}

func TestOnlyWhoMayProtectRestoresOverProtection(t *testing.T) {
	ctx := context.Background()
	w := &protectWorld{board: protectBoard(false, 1), admins: map[string]bool{adaUUID: true}}
	w.swap(t)
	restored := 0
	restore := func() error { restored++; return nil }

	if err := restoreRecorded(ctx, w.board, edUUID, nil, nil, restore); err != nil || restored != 1 || len(w.rows) != 0 {
		t.Fatalf("an editor restoring with nothing protected: %v, %d restores, rows %q", err, restored, w.rows)
	}
	if err := restoreRecorded(ctx, w.board, edUUID, []string{"e-1"}, nil, restore); !errors.Is(err, ErrCannotProtect) || restored != 1 {
		t.Fatalf("an editor restoring over protection: %v, %d restores", err, restored)
	}
	if want := "board.protect_refused: Refused restoring a version of ‘Launch plan’ over protected objects"; len(w.rows) != 1 || w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
	w.rows = nil
	if err := restoreRecorded(ctx, w.board, adaUUID, []string{"e-1"}, []string{"e-2", "e-3"}, restore); err != nil || restored != 2 {
		t.Fatalf("an admin restoring over protection: %v, %d restores", err, restored)
	}
	if want := "board.protect.restored: Restored a version of ‘Launch plan’: protection lifted from 1 object, and 2 objects protected again"; len(w.rows) != 1 || w.rows[0] != want {
		t.Fatalf("the log: %q, want %q", w.rows, want)
	}
	if m := w.metas[len(w.metas)-1]; m["lifted_ids"] != "e-1" || m["protected_ids"] != "e-2,e-3" {
		t.Fatalf("the row's metadata: %v", m)
	}
}

// CR-216 M1: a board whose canvas is in object storage but couldn't be fetched
// (resolveBoardState only logs that, leaving State "") is an error, never
// "nothing protected", or an editor's restore of a version from before a lock
// would lift it unrecorded.
func TestARestoreNeverReadsAnUnfetchedCanvasAsUnprotected(t *testing.T) {
	version := protectedState(t)
	if _, _, err := restoreProtectionOver(&dgraphStruct.DgraphBoard{StateKey: "boards/b1/state", State: ""}, version); err == nil {
		t.Fatal("a canvas in object storage that couldn't be fetched read as nothing protected")
	}
	if lifted, _, err := restoreProtectionOver(&dgraphStruct.DgraphBoard{StateKey: "boards/b1/state", State: protectedState(t, "e-1")}, version); err != nil || strings.Join(lifted, ",") != "e-1" {
		t.Fatalf("a fetched canvas: lifted %v, err %v; want e-1", lifted, err)
	}
	if _, again, err := restoreProtectionOver(nil, protectedState(t, "e-2")); err != nil || strings.Join(again, ",") != "e-2" {
		t.Fatalf("no board read: again %v, err %v; want e-2", again, err)
	}
	if lifted, again, err := restoreProtectionOver(&dgraphStruct.DgraphBoard{}, version); err != nil || len(lifted)+len(again) != 0 {
		t.Fatalf("a board with no canvas yet: %v %v %v; want nothing and no error", lifted, again, err)
	}
}
