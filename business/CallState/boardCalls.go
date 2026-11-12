package business

// A board's calls (miro-27, calls with the board): every call that showed a
// board on its stage (call_session_boards), the one that showed it latest
// first, with what each kept, for the board's Calls.
//
// WHO SEES WHAT. The board's own rule first (business/Board CanRead): a
// board that is private and not shared with them, deleted, or not there at
// all is ErrBoardNotFound, and says nothing of its calls. Then each call's
// own (MayReadAll, one read a page for its conversations): only a call whose
// conversation they can open, never one they were only let into by its invite,
// since its conversation, and so its transcript, stays out of their reach. An
// instant meeting has no conversation (no chat, and it keeps no transcript or
// recording), so a board lists conversations' calls: a channel's, a DM's or a
// group's. What a call
// kept is its conversation's, and its recordings and transcript open through
// the conversation's own routes, which ask again. Both rules are read afresh
// every time, so someone taken out of the conversation or off the board sees
// nothing of it on their next read. Never through an API token, never a bot
// (ErrBoardCallsRefused): a board's calls are read in the app, as yourself.

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/pagefill"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	// ErrBoardNotFound is a board that isn't there for this person (business/Board's).
	ErrBoardNotFound = boardBusiness.ErrBoardNotFound
	// ErrBoardCallsRefused is a board's calls asked for through an API token, or by a bot.
	ErrBoardCallsRefused = errors.New("callstate: a board's calls are read in the app, as yourself")
)

// MaxBoardCalls is the most calls a board lists.
const MaxBoardCalls = recordingDomain.MaxCallWindows

// maxBoardCallPages bounds a read of a board's calls at ten pages of
// MaxBoardCalls (300 calls, ten reads of the board's calls and at most ten of
// their conversations' rules): a board shown in many more calls its reader may
// not read lists the ones they may within those, a fixed amount of work.
const maxBoardCallPages = 10

// BoardCall is a call that showed a board, as its Calls list shows it.
type BoardCall struct {
	SessionID string `json:"session_id"`
	// Kind and Conversation are what the call belongs to (its Ref): a
	// channel, a DM or a group, whose call page and recordings open by it.
	Kind         string     `json:"kind"`
	Conversation string     `json:"conversation"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	// Live is a call going on now, and OnStage whether the board is on its stage.
	Live    bool `json:"live"`
	OnStage bool `json:"on_stage"`
	// ShownBy is who first put the board on its stage, by the name the call knew them by.
	ShownBy string    `json:"shown_by"`
	ShownAt time.Time `json:"shown_at"`
	People  int       `json:"people"`
	// Artefacts is what a finished conversation's call kept: its transcript
	// (on its session's node, with its notes and recap) and its recordings.
	Artefacts []recordingDomain.CallArtefact `json:"artefacts"`
}

// Seams for tests.
var (
	readableBoards = boardBusiness.ReadableBoardsFor
	callsOfBoard   = model.CallsOfBoard
	mayReadCalls   = MayReadAll
	callArtefacts  = recordingDomain.CallArtefacts
)

// readableCall is a call that showed a board, with what it belongs to, for
// someone who may read it.
type readableCall struct {
	row model.BoardCall
	ref Ref
}

// BoardCalls answers a board's calls as who may see them (above): the latest
// MaxBoardCalls they may read among the calls of the board's latest
// maxBoardCallPages pages.
func BoardCalls(ctx context.Context, who *userModels.UserInfo, board string) ([]BoardCall, error) {
	if who == nil || helpers.ActsThroughToken(ctx) || who.UserDgraphInfo.IsBot {
		return nil, ErrBoardCallsRefused
	}
	id, err := uuid.Parse(strings.TrimSpace(board))
	if err != nil {
		return nil, ErrBoardNotFound
	}
	me := who.UserDgraphInfo.Uuid
	if me == "" && who.UserPostgresInfo.Id != uuid.Nil {
		me = who.UserPostgresInfo.Id.String()
	}
	readable, err := readableBoards(ctx, []string{id.String()}, me)
	if err != nil {
		return nil, err
	}
	if !readable[id.String()] {
		return nil, ErrBoardNotFound
	}
	// The latest MaxBoardCalls calls they may read, never the latest
	// MaxBoardCalls with the ones they may not left out: pagefill.Fill reads
	// a page of MaxBoardCalls of the board's calls at a time, from right
	// after the last page's last call by its key (shown at, then session, so
	// calls shown at one instant neither repeat nor skip across a page's
	// edge), and keeps each page's calls they may read, each page's
	// conversations asked in one read (none asked twice), until there are
	// MaxBoardCalls, the calls run out or maxBoardCallPages pages are read.
	read := func(ctx context.Context, after model.BoardCallsAfter, size int) ([]model.BoardCall, model.BoardCallsAfter, error) {
		rows, err := callsOfBoard(ctx, id, after, size)
		if err != nil || len(rows) == 0 {
			return rows, after, err
		}
		return rows, rows[len(rows)-1].After(), nil
	}
	may, asked := map[Ref]Reading{}, map[Ref]bool{}
	keep := func(ctx context.Context, rows []model.BoardCall) ([]readableCall, error) {
		refs := make([]Ref, len(rows))
		var ask []Ref
		for i, row := range rows {
			if ref, _, ok := RefOfRoom(row.Room); ok && ref.Kind != KindMeeting {
				refs[i] = ref
				if !asked[ref] {
					asked[ref] = true
					ask = append(ask, ref)
				}
			}
		}
		if len(ask) > 0 {
			got, err := mayReadCalls(ctx, who, ask)
			if err != nil {
				return nil, err
			}
			maps.Copy(may, got)
		}
		out := make([]readableCall, 0, len(rows))
		for i, row := range rows {
			if r, ok := may[refs[i]]; refs[i].Kind != "" && ok && !r.Invitee {
				out = append(out, readableCall{row, refs[i]})
			}
		}
		return out, nil
	}
	rows, err := pagefill.Fill(ctx, read, keep, MaxBoardCalls, MaxBoardCalls, maxBoardCallPages)
	if err != nil {
		return nil, err
	}
	out := make([]BoardCall, 0, len(rows))
	keys := make([]string, 0, len(rows))
	var windows []recordingDomain.CallWindow
	for _, c := range rows {
		row, ref := c.row, c.ref
		out = append(out, BoardCall{SessionID: row.SessionID.String(), Kind: ref.Kind, Conversation: ref.ID,
			StartedAt: row.StartedAt, EndedAt: row.EndedAt, Live: row.EndedAt == nil, OnStage: row.OnStage,
			ShownBy: row.ShownBy, ShownAt: row.ShownAt, People: row.People, Artefacts: []recordingDomain.CallArtefact{}})
		key := ""
		if row.EndedAt != nil {
			key = helpers.CallSessionKeyFor(row.RoomSID)
		}
		if key != "" {
			windows = append(windows, recordingDomain.CallWindow{Key: key, Home: ref.ID,
				From: row.StartedAt.Add(-recordingDomain.CallWindowSlack), To: row.EndedAt.Add(recordingDomain.CallWindowSlack)})
		}
		keys = append(keys, key)
	}
	kept, err := callArtefacts(ctx, windows)
	if err != nil {
		return nil, err
	}
	for i, key := range keys {
		if a := kept[key]; key != "" && len(a) > 0 {
			out[i].Artefacts = a
		}
	}
	return out, nil
}
