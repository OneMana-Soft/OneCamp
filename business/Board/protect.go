package business

// Protected objects on a board (miro-44): Miro's Protected Lock, which is on
// its Enterprise plan only, here on every plan.
//
// The board's owner, or a workspace admin, protects objects on it, and only
// they lift the protection. The collaboration service holds the lock (its
// live/ops/protect.js): a record of what is protected, beside the elements,
// and a guard that puts back any tab's change to a protected object, its
// owner's included (protection is lifted first, as in Miro). Only this route
// writes that record, and only once its row is in the admin audit log (no
// record, no lock), so every lock and lift is in the log by construction. A
// refusal is a row of its own. A version restored over protection is the
// owner's or an admin's too, and its row says what it lifts and brings back.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/business/Board/scene"
	domain "github.com/akashc777/OneCamp/domain/Board"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
)

// MaxProtect is the most objects one request protects or lifts (the
// collaboration service's protect op takes as many).
const MaxProtect = 500

// protectedRoot is the record of a board's protected objects in its state.
const protectedRoot = "protected"

var (
	// ErrCannotProtect is a person who may not protect or lift objects on a
	// board, or restore a version over its protection. Its route answers 403.
	ErrCannotProtect = errors.New("only the board's owner and the workspace's admins can protect its objects or lift their protection")
	// ErrProtectIDs is a list of objects a request can't take. Its route
	// answers 400.
	ErrProtectIDs = fmt.Errorf("name between 1 and %d objects, each by its id", MaxProtect)
)

// protectSeams are where protecting reads, asks, writes and records: seams
// for tests.
var protectSeams = struct {
	read     func(ctx context.Context, boardUUID, userUID string) (*dgraphStruct.DgraphBoard, error)
	isAdmin  func(ctx context.Context, userUUID string) bool
	live     func(ctx context.Context, boardUUID string, ids []string, protect bool, by, name string) (int, error)
	recorded func(ctx context.Context, p auditBusiness.Principal, action, category, summary string, meta map[string]interface{}, change func() error) error
	refused  func(ctx context.Context, p auditBusiness.Principal, action, category, summary string, meta map[string]interface{}) error
}{
	read:    domain.GetBasicDgraphBoardByUUID,
	isAdmin: userDomain.IsWorkspaceAdmin,
	// Read at call time, so a swap of collab.ProtectBoard (its own seam) holds.
	live: func(ctx context.Context, boardUUID string, ids []string, protect bool, by, name string) (int, error) {
		return collab.ProtectBoard(ctx, boardUUID, ids, protect, by, name)
	},
	recorded: auditBusiness.RecordedAs,
	refused: func(ctx context.Context, p auditBusiness.Principal, action, category, summary string, meta map[string]interface{}) error {
		return auditBusiness.RecordForPrincipal(ctx, p.ID, p.Email, p.Kind, action, category, summary, p.Meta(meta))
	},
}

// CanProtect is who may protect objects on a board and lift their
// protection: its owner, or a workspace admin; nobody, once it is deleted.
// Pure: admin is whether the person is a workspace admin for this call
// (userDomain.IsWorkspaceAdmin, which an API token never is).
func CanProtect(board *dgraphStruct.DgraphBoard, userUUID string, admin bool) bool {
	if board == nil || isDeleted(board) {
		return false
	}
	return admin || (board.CreatedBy != nil && board.CreatedBy.Uuid != "" && board.CreatedBy.Uuid == userUUID)
}

// protectIDs is ids once each, in their order, or ErrProtectIDs.
func protectIDs(ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 64 {
			return nil, ErrProtectIDs
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 || len(out) > MaxProtect {
		return nil, ErrProtectIDs
	}
	return out, nil
}

// objects is how a row counts them: "1 object", "2 objects".
func objects(n int) string {
	if n == 1 {
		return "1 object"
	}
	return fmt.Sprintf("%d objects", n)
}

// isPrivateBoard is whether a row must not name the board: a private one
// (admins can't open it), or one whose privacy isn't known, as the board's
// other rows (boardAuditRow).
func isPrivateBoard(board *dgraphStruct.DgraphBoard) bool {
	return board.IsPrivate == nil || *board.IsPrivate
}

// boardWords is how a row names a board.
func boardWords(board *dgraphStruct.DgraphBoard) string {
	if isPrivateBoard(board) {
		return "a private board"
	}
	if title := strings.TrimSpace(board.Title); title != "" {
		return "‘" + title + "’"
	}
	return "a board"
}

// protectMeta is a protection row's metadata: the board, and its title only
// when it isn't private.
func protectMeta(board *dgraphStruct.DgraphBoard) map[string]interface{} {
	meta := map[string]interface{}{"resource_kind": KindBoard, "resource_uuid": board.Uuid,
		"at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	for k, v := range boardEventData(board) {
		if k == "title" && isPrivateBoard(board) {
			continue
		}
		meta[k] = v
	}
	return meta
}

// mayProtect is ErrBoardNotFound for one who can't read the board, then
// ErrCannotProtect, with its refusal row, for one who may not protect on it.
func mayProtect(ctx context.Context, board *dgraphStruct.DgraphBoard, userUUID, refusal string, meta map[string]interface{}) error {
	if !CanRead(board, userUUID) {
		return ErrBoardNotFound
	}
	if CanProtect(board, userUUID, protectSeams.isAdmin(ctx, userUUID)) {
		return nil
	}
	if err := protectSeams.refused(ctx, auditBusiness.ContextPrincipal(ctx, ""), "board.protect_refused", auditBusiness.CategoryBoards, refusal, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Board: a refused protection wasn't recorded: %+v", err)
	}
	return ErrCannotProtect
}

// ProtectElements protects the objects ids on a board (protect true) or lifts
// their protection (false), as the person userUUID (name is how the board
// names them). A board they can't read is ErrBoardNotFound and leaves no
// row; one they may not protect on is ErrCannotProtect, recorded as refused.
// The lock or lift is made only once its row is written (wrapping
// ErrNotRecorded if it can't be), and a live op that fails is a _failed row.
// Answers how many of the objects changed.
func ProtectElements(ctx context.Context, boardUUID string, ids []string, protect bool, userUID, userUUID, name string) (int, error) {
	ids, err := protectIDs(ids)
	if err != nil {
		return 0, err
	}
	board, err := protectSeams.read(ctx, boardUUID, userUID)
	if errors.Is(err, helpers.ErrNotFound) || (err == nil && board == nil) {
		return 0, ErrBoardNotFound
	}
	if err != nil {
		return 0, err
	}
	what, where := objects(len(ids)), " on "+boardWords(board)
	action, summary, refusal := "board.protect.locked", "Protected "+what+where, "Refused protecting "+what+where
	if !protect {
		action, summary, refusal = "board.protect.lifted", "Lifted protection from "+what+where, "Refused lifting protection from "+what+where
	}
	meta := protectMeta(board)
	meta["count"], meta["element_ids"] = len(ids), strings.Join(ids, ",")
	if err := mayProtect(ctx, board, userUUID, refusal, meta); err != nil {
		return 0, err
	}
	changed := 0
	err = protectSeams.recorded(ctx, auditBusiness.ContextPrincipal(ctx, ""), action, auditBusiness.CategoryBoards, summary, meta, func() error {
		n, lerr := protectSeams.live(ctx, board.Uuid, ids, protect, userUUID, name)
		changed = n
		return lerr
	})
	return changed, err
}

// protectedIDs is the ids a board state holds protected, sorted.
func protectedIDs(stateB64 string) ([]string, error) {
	roots, err := scene.StateRoots(stateB64)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(roots[protectedRoot]))
	for id := range roots[protectedRoot] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// restoreProtectionOver is restoreProtection over the board as it is now
// (cur, from the business read, its canvas resolved from object storage). A
// canvas kept in object storage that couldn't be fetched (resolveBoardState
// only logs that, leaving State "") is an error, never "nothing protected"
// (CR-216 M1): a restore never lifts protection it couldn't see.
func restoreProtectionOver(cur *dgraphStruct.DgraphBoard, version string) (lifted, again []string, err error) {
	now := ""
	if cur != nil {
		if cur.State == "" && cur.StateKey != "" {
			return nil, nil, errors.New("the board as it is: its canvas couldn't be fetched")
		}
		now = cur.State
	}
	return restoreProtection(now, version)
}

// restoreProtection is what restoring the state version over the state now
// does to protection: the objects it lifts it from (protected now, not in the
// version) and those it protects again (in the version, not now), each sorted.
// A state that can't be read is an error, never "nothing protected".
func restoreProtection(now, version string) (lifted, again []string, err error) {
	was, err := protectedIDs(now)
	if err != nil {
		return nil, nil, fmt.Errorf("the board as it is: %w", err)
	}
	will, err := protectedIDs(version)
	if err != nil {
		return nil, nil, fmt.Errorf("the version: %w", err)
	}
	return missingFrom(was, will), missingFrom(will, was), nil
}

// missingFrom is the ids of a that b doesn't have, in a's order.
func missingFrom(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, id := range b {
		in[id] = true
	}
	var out []string
	for _, id := range a {
		if !in[id] {
			out = append(out, id)
		}
	}
	return out
}

// restoreRecorded runs restore, a version's restore over the board as it is.
// One that changes no protection is the editor's as ever, with no row of its
// own. One that lifts or brings back protection is the owner's or an
// admin's, made only once its row, naming what it lifts and brings back, is
// written; anyone else is ErrCannotProtect, recorded as refused.
func restoreRecorded(ctx context.Context, board *dgraphStruct.DgraphBoard, userUUID string, lifted, again []string, restore func() error) error {
	if len(lifted) == 0 && len(again) == 0 {
		return restore()
	}
	meta := protectMeta(board)
	meta["lifted_ids"], meta["protected_ids"] = strings.Join(lifted, ","), strings.Join(again, ",")
	if err := mayProtect(ctx, board, userUUID, "Refused restoring a version of "+boardWords(board)+" over protected objects", meta); err != nil {
		return err
	}
	var parts []string
	if len(lifted) > 0 {
		parts = append(parts, "protection lifted from "+objects(len(lifted)))
	}
	if len(again) > 0 {
		parts = append(parts, objects(len(again))+" protected again")
	}
	summary := "Restored a version of " + boardWords(board) + ": " + strings.Join(parts, ", and ")
	return protectSeams.recorded(ctx, auditBusiness.ContextPrincipal(ctx, ""), "board.protect.restored", auditBusiness.CategoryBoards, summary, meta, restore)
}
