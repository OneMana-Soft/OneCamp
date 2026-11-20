package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// ownedBoards is the boards a person owns (made: board_created_by), live or in
// the trash, never a scratch board (one the app made and took away again).
const ownedBoards = `~board_created_by @filter(has(board_uuid) AND NOT eq(board_scratch, true))`

// BoardsOwnedBy is up to first of the boards a person owns, live or in the
// trash, and how many there are: for handing a departed member's boards on
// with their docs (docs C6-2, miro-32): the trash too, since its owner is who
// restores a board.
func BoardsOwnedBy(ctx context.Context, userUUID string, first int) ([]string, int, error) {
	if first < 1 {
		first = 1
	}
	query := fmt.Sprintf(`query owned($owner: string) {
		var(func: eq(user_uuid, $owner)) {
			theirs as %s
		}
		board_count(func: uid(theirs)) {
			count: count(uid)
		}
		boards(func: uid(theirs), orderasc: board_uuid, first: %d) {
			board_uuid
		}
	}`, ownedBoards, first)
	out, err := dgexec.Read[struct {
		Count []struct {
			Count int `json:"count"`
		} `json:"board_count"`
		Boards []struct {
			UUID string `json:"board_uuid"`
		} `json:"boards"`
	}](ctx, query, map[string]string{"$owner": userUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BoardsOwnedBy err: %+v", err)
		return nil, 0, err
	}
	ids := make([]string, 0, len(out.Boards))
	for _, b := range out.Boards {
		if b.UUID != "" {
			ids = append(ids, b.UUID)
		}
	}
	total := 0
	if len(out.Count) > 0 {
		total = out.Count[0].Count
	}
	return ids, total, nil
}

// BoardsOwnedCounts is how many boards each person owns, by BoardsOwnedBy's
// rule, in one read for every person: a person who owns none is left out.
// Keys are lower-case uuids.
func BoardsOwnedCounts(ctx context.Context, people []uuid.UUID) (map[string]int, error) {
	counts := map[string]int{}
	list := dgraphquery.UUIDListOf(people)
	if list == "" {
		return counts, nil
	}
	query := `{
		people(func: eq(user_uuid, [` + list + `])) {
			user_uuid
			made: ` + ownedBoards + ` { n: count(uid) }
		}
	}`
	out, err := dgexec.Read[struct {
		People []struct {
			UUID string `json:"user_uuid"`
			Made []struct {
				N int `json:"n"`
			} `json:"made"`
		} `json:"people"`
	}](ctx, query, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BoardsOwnedCounts err: %+v", err)
		return nil, err
	}
	for _, p := range out.People {
		for _, c := range p.Made {
			if c.N > 0 {
				counts[strings.ToLower(p.UUID)] += c.N
			}
		}
	}
	return counts, nil
}

// HandOnBoard makes toUUID the board's owner in one write (miro-32, docs
// C6-2): the owner before (fromUUID, "" for none) stays on as a co-owner, an
// editor too; the new owner loses any other role they held, being the owner.
// People are named by their uuids, as query variables. helpers.ErrNotFound
// when the board or the new owner isn't there.
func HandOnBoard(ctx context.Context, boardUUID, fromUUID, toUUID string) error {
	query := `query handOn($b: string, $to: string, $from: string) {
		b as var(func: eq(board_uuid, $b))
		to as var(func: ` + dgraphquery.PersonByUUID("$to") + `)
		toall as var(func: eq(user_uuid, $to))
		from as var(func: ` + dgraphquery.PersonByUUID("$from") + `)
		heir(func: uid(to)) { uid }
		board(func: uid(b)) { uid }
	}`
	give := dgexec.Mutation{
		Cond:      "@if(eq(len(b), 1) AND eq(len(to), 1))",
		SetNQuads: "uid(b) <board_created_by> uid(to) .",
		DelNQuads: "uid(b) <board_editing_users> uid(toall) .\nuid(b) <board_reading_users> uid(toall) .\n" +
			"uid(b) <board_commenting_users> uid(toall) .\nuid(b) <board_owning_users> uid(toall) .",
	}
	keep := dgexec.Mutation{
		Cond:      "@if(eq(len(b), 1) AND eq(len(to), 1) AND eq(len(from), 1))",
		SetNQuads: "uid(b) <board_editing_users> uid(from) .\nuid(b) <board_owning_users> uid(from) .",
	}
	resp, err := dgexec.Mutate(ctx, query, map[string]string{"$b": boardUUID, "$to": toUUID, "$from": fromUUID}, give, keep)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HandOnBoard err: %+v", err)
		return err
	}
	// The query's own answer says whether the board and the new owner were
	// there, so the write that was made says so too.
	if resp == nil || !strings.Contains(string(resp.Json), `"board":[{`) || !strings.Contains(string(resp.Json), `"heir":[{`) {
		return helpers.ErrNotFound
	}
	return nil
}
