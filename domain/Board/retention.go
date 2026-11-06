package domain

// Boards under retention rules and holds (miro-44 head R; main's ruling, 3 Nov
// 20:3x), as the graph answers them: which boards a rule removes, removing
// them, and a hold's people's boards. business/Board/retention.go is the kind.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// TrashedBoard is a board a retention rule may remove, as TrashedBoardsBefore
// reads it: what the purge checks it by again (its deletion, its maker and
// editors).
type TrashedBoard = dgraphStruct.DgraphBoard

// trashedBoardsFilter is the boards a rule with cutoff removes: in the trash
// (dgraphquery.Deleted: a live board's deletion time is 0001-01-01 or the
// epoch) since before cutoff, and made or edited by nobody held. vars are the
// held people's boards, for the filter's NOT; with nobody held there are none,
// as Dgraph refuses a variable a query never uses. Pure.
func trashedBoardsFilter(cutoff time.Time, held []uuid.UUID) (vars, filter string) {
	filter = "type(Board) AND " + dgraphquery.Deleted("board_deleted_at") +
		" AND lt(board_deleted_at, " + strconv.Quote(cutoff.UTC().Format(time.RFC3339Nano)) + ")"
	if list := dgraphquery.UUIDListOf(held); list != "" {
		vars = `var(func: eq(user_uuid, [` + list + `])) {
				heldMade as ~board_created_by
				heldEdits as ~board_editing_users
			}`
		filter += " AND NOT uid(heldMade) AND NOT uid(heldEdits)"
	}
	return vars, filter
}

// CountTrashedBoardsBefore is how many boards a rule with cutoff would remove
// now, held people's kept.
func CountTrashedBoardsBefore(ctx context.Context, cutoff time.Time, held []uuid.UUID) (int64, error) {
	vars, filter := trashedBoardsFilter(cutoff, held)
	out, err := dgexec.Read[struct {
		N []struct {
			Count int64 `json:"count"`
		} `json:"n"`
	}](ctx, `{
			`+vars+`
			n(func: type(Board)) @filter(`+filter+`) { count(uid) }
		}`, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CountTrashedBoardsBefore err: %+v", err)
		return 0, err
	}
	if len(out.N) == 0 {
		return 0, nil
	}
	return out.N[0].Count, nil
}

// TrashedBoardsBefore reads at most limit of the boards a rule with cutoff
// removes, held people's kept.
func TrashedBoardsBefore(ctx context.Context, cutoff time.Time, held []uuid.UUID, limit int) ([]*TrashedBoard, error) {
	vars, filter := trashedBoardsFilter(cutoff, held)
	boards, err := dgexec.List[*TrashedBoard](ctx, `{
			`+vars+`
			boards(func: type(Board), first: `+strconv.Itoa(limit)+`) @filter(`+filter+`) {
				uid
				board_uuid
				board_deleted_at
				board_created_by { uid user_uuid }
				board_editing_users { uid user_uuid }
			}
		}`, nil, "boards")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TrashedBoardsBefore err: %+v", err)
	}
	return boards, err
}

// PurgeBoardNodes removes boards for good: of the nodes uids names, those the
// rule still removes when the delete runs (asked again in the same request, so
// a board restored or put on hold since it was read stays), with their
// comments, and the links tasks and projects held to them.
func PurgeBoardNodes(ctx context.Context, uids []string, cutoff time.Time, held []uuid.UUID) error {
	var ok []string
	for _, u := range uids {
		if helpers.IsDgraphUID(u) {
			ok = append(ok, u)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	vars, filter := trashedBoardsFilter(cutoff, held)
	_, err := dgexec.Mutate(ctx, `query {
			`+vars+`
			gone as var(func: uid(`+strings.Join(ok, ", ")+`)) @filter(`+filter+`)
			var(func: uid(gone)) {
				notes as ~comment_board
				linkers as ~linked_boards
			}
		}`, nil, dgexec.Mutation{
		DelNQuads: "uid(gone) * * .\nuid(notes) * * .\nuid(linkers) <linked_boards> uid(gone) .\n",
		Cond:      "@if(gt(len(gone), 0))",
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/PurgeBoardNodes err: %+v", err)
	}
	return err
}

// BoardUUIDsInGraph is which of the boards ids names the graph still holds,
// in the trash or not.
func BoardUUIDsInGraph(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return out, nil
	}
	boards, err := dgexec.List[dgraphStruct.DgraphBoard](ctx, `{
			boards(func: eq(board_uuid, [`+list+`])) { board_uuid }
		}`, nil, "boards")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BoardUUIDsInGraph err: %+v", err)
		return nil, err
	}
	for _, b := range boards {
		out[b.Uuid] = true
	}
	return out, nil
}

// HeldBoardQuery narrows a hold's boards: Text, an escaped term the title
// matches (dgraphquery.SanitizeSearchTerm); a stretch of time a board was
// alive in (made by To, changed since From; zero: open); and After, the board
// id a part of an export starts after.
type HeldBoardQuery struct {
	Text     string
	From, To time.Time
	After    string
}

// heldPerson is a person on a board, as a hold's search and export name them.
const heldPerson = "{ user_uuid user_name user_full_name }"

// heldBoardsQuery is the query HeldBoards runs. Pure.
func heldBoardsQuery(people []uuid.UUID, q HeldBoardQuery, limit int, full bool) string {
	filter := "type(Board)"
	if q.Text != "" {
		filter += " AND regexp(board_title, /.*" + q.Text + ".*/i)"
	}
	if !q.From.IsZero() {
		filter += " AND ge(board_updated_at, " + strconv.Quote(q.From.UTC().Format(time.RFC3339Nano)) + ")"
	}
	if !q.To.IsZero() {
		filter += " AND le(board_created_at, " + strconv.Quote(q.To.UTC().Format(time.RFC3339Nano)) + ")"
	}
	if q.After != "" {
		filter += " AND gt(board_uuid, " + strconv.Quote(q.After) + ")"
	}
	state := ""
	if full {
		state = `
				board_state
				board_state_key
				board_reading_users ` + heldPerson + `
				board_commenting_users ` + heldPerson
	}
	return fmt.Sprintf(`{
			var(func: eq(user_uuid, [%s])) {
				made as ~board_created_by
				edits as ~board_editing_users
			}
			boards(func: uid(made, edits), orderasc: board_uuid, first: %d) @filter(%s) {
				uid
				board_uuid
				board_title
				board_snippet
				board_private
				board_created_at
				board_updated_at
				board_deleted_at
				board_created_by %s
				board_editing_users %s%s
			}
		}`, dgraphquery.UUIDListOf(people), limit, filter, heldPerson, heldPerson, state)
}

// HeldBoards is the boards people made or edit, deleted ones included, that
// q matches, at most limit, in their id's order: with their state and every
// share when full (an export), or what a list shows.
func HeldBoards(ctx context.Context, people []uuid.UUID, q HeldBoardQuery, limit int, full bool) ([]*dgraphStruct.DgraphBoard, error) {
	if dgraphquery.UUIDListOf(people) == "" || limit <= 0 {
		return nil, nil
	}
	boards, err := dgexec.List[*dgraphStruct.DgraphBoard](ctx, heldBoardsQuery(people, q, limit, full), nil, "boards")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HeldBoards err: %+v", err)
	}
	return boards, err
}
