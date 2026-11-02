package business

// What a board says, in search (W6-B1, miro-05): its frames' names
// and the words of its notes, shapes, arrows and text (scene.TextItems), as one
// outline. Written after its saves, at most once per board per few seconds
// with its newest state, and only when the words changed, on its search entry
// (board_text). Boards drawn before board text existed are written by
// the backfill, a page at a time. One writer does it, for any number of
// boards, with each read and write made once for all of them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/business/Board/scene"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	// searchTextMax caps a board's text, in runes: search keeps the words of
	// a big board.
	searchTextMax = 200_000
	// boardTextWait is how long a board's text waits after a save, so a
	// stretch of drawing writes it once.
	boardTextWait = 5 * time.Second
	// boardTextDoneKey records the backfill done on this install.
	boardTextDoneKey = "board.text_backfilled"
	// boardTextPage is how many boards the backfill writes at a time: their
	// states are read together, so it bounds what a page holds.
	boardTextPage = 50
	// boardTextMost bounds one backfill pass: a longer list goes on at the next.
	boardTextMost = 200_000
)

// boardTexts writes a board's text after its saves: one run per board at a
// time, with the state saved last.
var boardTexts = helpers.NewLatestByKey("board text", boardTextWait, indexBoardText)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "board.text", Every: 6 * time.Hour, RunAtStart: true, Run: BackfillBoardText})
}

// The text's reads and writes, each for several boards in one request. Seams
// for tests.
var (
	writeBoardTexts = openSearchBoardModels.SetBoardTextsInOpenSearch
	// boardTextDigests is each board's digest as last written, "" for none.
	boardTextDigests = func(ctx context.Context, boardUUIDs []string) []string {
		keys := make([]redisStore.CounterKey, len(boardUUIDs))
		for i, id := range boardUUIDs {
			keys[i] = redisStore.CounterKey{Spec: registry.BoardTextDigest, Args: []string{id}}
		}
		d, _, _ := redisStore.GetStringMany(ctx, keys)
		return d
	}
	keepBoardTextDigests = func(ctx context.Context, digests map[string]string) error {
		sets := make([]redisStore.StringSet, 0, len(digests))
		for id, d := range digests {
			sets = append(sets, redisStore.StringSet{Key: redisStore.CounterKey{Spec: registry.BoardTextDigest, Args: []string{id}}, Value: d})
		}
		return redisStore.SetStringMany(ctx, sets)
	}
	liveBoardPage = domain.GetLiveBoardUUIDPage
	// boardsForText reads several boards' states and sharing: one graph read,
	// then the objects of the states kept in object storage.
	boardsForText = func(ctx context.Context, boardUUIDs []string) ([]*dgraphStruct.DgraphBoard, error) {
		boards, err := domain.GetBoardsForText(ctx, boardUUIDs)
		if err == nil {
			resolveBoardStates(ctx, boards)
		}
		return boards, err
	}
	boardTextDone     = configModel.IsDone
	markBoardTextDone = func() error { return configModel.UpsertConfig(boardTextDoneKey, "done") }
)

// boardTextSource is a board whose text is to be written: its state, and who
// may read it (GetBoardPermissions' fields).
type boardTextSource struct {
	uuid, state string
	perms       *dgraphStruct.DgraphBoard
}

// boardAccess is who reaches a board, as its comments' search entries carry it.
type boardAccess struct {
	Public                       bool
	Reading, Editing, Commenting []string
	CreatedBy                    string
}

// boardAccessOf is who reaches a board, as perms (GetBoardPermissions) holds
// it, for its comments' search entries.
func boardAccessOf(perms *dgraphStruct.DgraphBoard) boardAccess {
	a := boardAccess{
		Public:     perms.IsPrivate == nil || !*perms.IsPrivate,
		Reading:    boardUserUUIDs(perms.ReadingUser),
		Editing:    boardUserUUIDs(perms.EditingUser),
		Commenting: boardUserUUIDs(perms.CommentingUser),
	}
	if perms.CreatedBy != nil {
		a.CreatedBy = perms.CreatedBy.Uuid
	}
	return a
}

// indexBoardText is boardTexts' run: a failure is logged, and the board's next
// save, or the backfill, writes it again.
func indexBoardText(boardUUID, stateB64 string) {
	ctx, cancel := context.WithTimeout(context.Background(), boardRewriteTimeout)
	defer cancel()
	if err := writeBoardTextNow(ctx, boardUUID, stateB64); err != nil {
		helpers.LogErrorWithContext(ctx, "business/indexBoardText %s: %v", boardUUID, err)
	}
}

// writeBoardTextNow writes what one board says, from its state, as
// writeBoardTextsNow does. Who may read the board is read first: a board that
// is gone or deleted is never written.
func writeBoardTextNow(ctx context.Context, boardUUID, stateB64 string) error {
	perms, err := readBoardSharing(ctx, boardUUID)
	if err != nil || perms == nil {
		return fmt.Errorf("could not read the board's sharing: %v", err)
	}
	return writeBoardTextsNow(ctx, []boardTextSource{{uuid: boardUUID, state: stateB64, perms: perms}})[boardUUID]
}

// boardTextDigestOf is what a board's text was written from: its title and its
// text, in the AI edition's form with the AI off, so a move between the
// editions writes no board again.
func boardTextDigestOf(title, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%t\x00%s\x00%s", false, title, text)))
	return hex.EncodeToString(sum[:])
}

// writeBoardTextsNow writes what each board says onto its search entry, unless
// it said the same when last written; a deleted board's entry is marked so
// already, and it is left. The boards' digests are read in one request, and
// their search entries and their digests are written in one request each. It
// answers the boards it couldn't write, and
// why: when a request fails, none of its boards keeps a digest, so the next
// save or backfill pass writes each again.
func writeBoardTextsNow(ctx context.Context, sources []boardTextSource) map[string]error {
	failed := map[string]error{}
	ids := make([]string, len(sources))
	for i, s := range sources {
		ids[i] = s.uuid
	}
	stored := boardTextDigests(ctx, ids)
	var texts []openSearchBoardModels.BoardText
	digests := map[string]string{}
	for i, s := range sources {
		roots, err := scene.StateRoots(s.state)
		if err != nil {
			failed[s.uuid] = err
			continue
		}
		if s.perms == nil {
			failed[s.uuid] = errors.New("could not read the board's sharing")
			continue
		}
		if s.perms.DeletedAt != nil && s.perms.DeletedAt.Year() > 1970 {
			continue
		}
		items := scene.TextItems(roots["elements"])
		text := scene.Outline(items, searchTextMax)
		digest := boardTextDigestOf(s.perms.Title, text)
		if i < len(stored) && stored[i] == digest {
			continue
		}
		texts = append(texts, openSearchBoardModels.BoardText{BoardUUID: s.uuid, Text: text})
		/* AI call omitted in v1 */
		digests[s.uuid] = digest
	}
	if len(digests) == 0 {
		return failed
	}
	if err := writeBoardTexts(ctx, texts); err != nil {
		return failedAll(failed, digests, fmt.Errorf("could not write the board's text to search: %w", err))
	}
	if err := keepBoardTextDigests(ctx, digests); err != nil {
		return failedAll(failed, digests, fmt.Errorf("could not keep the board's text digest: %w", err))
	}
	return failed
}

// failedAll adds every board a failed request carried to failed.
func failedAll(failed map[string]error, digests map[string]string, err error) map[string]error {
	for id := range digests {
		failed[id] = err
	}
	return failed
}

// BackfillBoardText writes the text of every live board that has none written
// (no digest): boards drawn before board text existed. It goes a page of
// boards at a time (backfillBoardTextPage). Run at start and every six hours
// until a pass writes every board; recorded then.
func BackfillBoardText(ctx context.Context) error {
	if done, err := boardTextDone(boardTextDoneKey); err != nil || done {
		return err
	}
	complete := true
	more, err := dgraphquery.EachPage(boardTextPage, boardTextMost, func(p dgraphquery.Page) ([]string, error) {
		return liveBoardPage(ctx, p.Offset, p.First)
	}, func(ids []string) error {
		if !backfillBoardTextPage(ctx, ids) {
			complete = false
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("board text backfill: listing boards: %w", err)
	}
	if more || !complete {
		return nil // the next pass writes what this one couldn't
	}
	return markBoardTextDone()
}

// backfillBoardTextPage writes the text of a page's boards that have none
// written: their digests in one read, those boards' states and sharing in one
// more (boardsForText), and their text as writeBoardTextsNow writes it. It
// says whether every board of the page now has its text.
func backfillBoardTextPage(ctx context.Context, ids []string) bool {
	stored := boardTextDigests(ctx, ids)
	var todo []string
	for i, id := range ids {
		if i >= len(stored) || stored[i] == "" {
			todo = append(todo, id)
		}
	}
	if len(todo) == 0 {
		return true
	}
	boards, err := boardsForText(ctx, todo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/BackfillBoardText: reading %d boards: %v", len(todo), err)
		return false
	}
	read := make(map[string]*dgraphStruct.DgraphBoard, len(boards))
	for _, b := range boards {
		if b != nil && read[b.Uuid] == nil {
			read[b.Uuid] = b
		}
	}
	complete := true
	sources := make([]boardTextSource, 0, len(todo))
	for _, id := range todo {
		b := read[id]
		if b == nil || (b.State == "" && b.StateKey != "") {
			// Gone since its page was listed, or its state's object unread.
			complete = false
			helpers.LogErrorWithContext(ctx, "business/BackfillBoardText %s: could not read the board's state", id)
			continue
		}
		sources = append(sources, boardTextSource{uuid: id, state: b.State, perms: b})
	}
	failed := writeBoardTextsNow(ctx, sources)
	for id, err := range failed {
		complete = false
		helpers.LogErrorWithContext(ctx, "business/BackfillBoardText %s: %v", id, err)
	}
	return complete
}
