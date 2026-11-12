package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A board's calls ask the board's rule first, then every conversation's in
// one read, list only the calls whose conversation the reader can open (never
// as a call's invitee, never a meeting, which has none), and read what
// finished calls kept by their sessions' keys in their windows; a live call
// keeps nothing yet. A token, a bot and a board they can't read get nothing.
func TestABoardsCallsAreTheOnesItsReaderMaySee(t *testing.T) {
	board := uuid.New()
	channel, other, dm := uuid.NewString(), uuid.NewString(), uuid.NewString()+" "+uuid.NewString()
	now := time.Now()
	ended := now.Add(-time.Hour)
	rows := []model.BoardCall{
		{SessionID: uuid.New(), Room: channel, RoomSID: "RM_live", StartedAt: now.Add(-time.Minute), OnStage: true, ShownBy: "Ana"},
		{SessionID: uuid.New(), Room: channel, RoomSID: "RM_one", StartedAt: ended.Add(-time.Hour), EndedAt: &ended, ShownBy: "Sam", People: 3},
		{SessionID: uuid.New(), Room: other, RoomSID: "RM_two", StartedAt: ended.Add(-time.Hour), EndedAt: &ended},
		{SessionID: uuid.New(), Room: dm, RoomSID: "RM_three", StartedAt: ended.Add(-time.Hour), EndedAt: &ended},
		{SessionID: uuid.New(), Room: "meet-" + uuid.NewString(), RoomSID: "RM_four", StartedAt: ended.Add(-time.Hour), EndedAt: &ended},
		{SessionID: uuid.New(), Room: "not a call room", StartedAt: ended},
	}
	defer func(r, c, m, a any) {
		readableBoards = r.(func(context.Context, []string, string) (map[string]bool, error))
		callsOfBoard = c.(func(context.Context, uuid.UUID, model.BoardCallsAfter, int) ([]model.BoardCall, error))
		mayReadCalls = m.(func(context.Context, *userModels.UserInfo, []Ref) (map[Ref]Reading, error))
		callArtefacts = a.(func(context.Context, []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error))
	}(readableBoards, callsOfBoard, mayReadCalls, callArtefacts)
	readable := true
	readableBoards = func(_ context.Context, ids []string, user string) (map[string]bool, error) {
		if len(ids) != 1 || ids[0] != board.String() || user != "ana-uuid" {
			t.Fatalf("the board's rule asked of %v for %q", ids, user)
		}
		return map[string]bool{ids[0]: readable}, nil
	}
	callsOfBoard = func(_ context.Context, b uuid.UUID, after model.BoardCallsAfter, limit int) ([]model.BoardCall, error) {
		if b != board || limit != MaxBoardCalls || after != (model.BoardCallsAfter{}) {
			t.Fatalf("calls of %s, %d, after %+v", b, limit, after)
		}
		return rows, nil
	}
	reads := 0
	asked := map[string]int{}
	mayReadCalls = func(_ context.Context, _ *userModels.UserInfo, refs []Ref) (map[Ref]Reading, error) {
		reads++
		out := map[Ref]Reading{}
		for _, ref := range refs {
			asked[ref.ID]++
			switch {
			case ref.ID == channel:
				out[ref] = Reading{}
			case ref.ID == dm, ref.Kind == KindMeeting:
				out[ref] = Reading{Invitee: ref.ID == dm} // the DM: let in by the call's invite only
			}
		}
		return out, nil
	}
	var windows []recordingDomain.CallWindow
	callArtefacts = func(_ context.Context, w []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error) {
		windows = w
		return map[string][]recordingDomain.CallArtefact{helpers.CallSessionKeyFor("RM_one"): {{Key: "EG_1", Recorded: true}}}, nil
	}
	ana := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "ana-uuid"}}

	calls, err := BoardCalls(context.Background(), ana, board.String())
	if err != nil || len(calls) != 2 {
		t.Fatalf("Ana's board calls: %+v, %v; want the channel's two", calls, err)
	}
	if !calls[0].Live || !calls[0].OnStage || calls[0].Kind != "channel" || len(calls[0].Artefacts) != 0 {
		t.Fatalf("the live call: %+v", calls[0])
	}
	if calls[1].Live || calls[1].People != 3 || calls[1].ShownBy != "Sam" || len(calls[1].Artefacts) != 1 || calls[1].Artefacts[0].Key != "EG_1" {
		t.Fatalf("the finished call: %+v", calls[1])
	}
	if reads != 1 || len(asked) != 3 || asked[other] != 1 || asked[dm] != 1 {
		t.Fatalf("the conversations' rules asked in one read, never a meeting's: %d reads of %v", reads, asked)
	}
	if len(windows) != 1 || windows[0].Key != helpers.CallSessionKeyFor("RM_one") || windows[0].Home != channel ||
		!windows[0].From.Equal(rows[1].StartedAt.Add(-recordingDomain.CallWindowSlack)) || !windows[0].To.Equal(ended.Add(recordingDomain.CallWindowSlack)) {
		t.Fatalf("what the calls kept was read for %+v", windows)
	}

	readable = false
	if _, err := BoardCalls(context.Background(), ana, board.String()); !errors.Is(err, ErrBoardNotFound) {
		t.Fatalf("a board Ana can't read: %v", err)
	}
	if _, err := BoardCalls(context.Background(), ana, "not-a-board"); !errors.Is(err, ErrBoardNotFound) {
		t.Fatalf("no board: %v", err)
	}
	bot := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "bot-uuid", IsBot: true}}
	for name, c := range map[string]struct {
		ctx context.Context
		who *userModels.UserInfo
	}{"a token": {helpers.WithActsThroughToken(context.Background()), ana}, "a bot": {context.Background(), bot}, "nobody": {context.Background(), nil}} {
		if _, err := BoardCalls(c.ctx, c.who, board.String()); !errors.Is(err, ErrBoardCallsRefused) {
			t.Fatalf("%s reads a board's calls: %v", name, err)
		}
	}
}

// boardPages answers rows a page at a time, as CallsOfBoard does: from the
// row after's key names, at most limit. Each page's start is noted in starts.
func boardPages(t *testing.T, rows []model.BoardCall, starts *[]model.BoardCallsAfter) func(context.Context, uuid.UUID, model.BoardCallsAfter, int) ([]model.BoardCall, error) {
	return func(_ context.Context, _ uuid.UUID, after model.BoardCallsAfter, limit int) ([]model.BoardCall, error) {
		*starts = append(*starts, after)
		from := 0
		if !after.ShownAt.IsZero() {
			from = slices.IndexFunc(rows, func(r model.BoardCall) bool {
				return r.SessionID == after.Session && r.ShownAt.Equal(after.ShownAt)
			}) + 1
			if from == 0 {
				t.Fatalf("a page after %+v, a row there isn't", after)
			}
		}
		return rows[from:min(from+limit, len(rows))], nil
	}
}

// boardRows is n ended calls in the conversation of, shown one a minute
// apart going back from at, the latest first.
func boardRows(n int, at time.Time, of func(i int) string) []model.BoardCall {
	out := make([]model.BoardCall, n)
	for i := range out {
		shown := at.Add(-time.Duration(i) * time.Minute)
		ended := shown.Add(30 * time.Minute)
		out[i] = model.BoardCall{SessionID: uuid.New(), Room: of(i), RoomSID: "RM_" + uuid.NewString()[:8],
			StartedAt: shown.Add(-time.Minute), EndedAt: &ended, ShownAt: shown}
	}
	return out
}

// A board lists the latest MaxBoardCalls calls its reader MAY read, however
// many calls they may not read were shown it since: it reads on a page at a
// time, from where the last page ended, asking each conversation's rule once
// (one read a page at most), until it has MaxBoardCalls or the calls run out,
// and reads what they all kept in one go.
func TestABoardsCallsAreTheLatestItsReaderMayRead(t *testing.T) {
	board := uuid.New()
	mine := uuid.NewString()
	theirs := func(int) string { return uuid.NewString() } // a private channel each, not the reader's
	ours := func(int) string { return mine }
	now := time.Now()
	defer func(r, c, m, a any) {
		readableBoards = r.(func(context.Context, []string, string) (map[string]bool, error))
		callsOfBoard = c.(func(context.Context, uuid.UUID, model.BoardCallsAfter, int) ([]model.BoardCall, error))
		mayReadCalls = m.(func(context.Context, *userModels.UserInfo, []Ref) (map[Ref]Reading, error))
		callArtefacts = a.(func(context.Context, []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error))
	}(readableBoards, callsOfBoard, mayReadCalls, callArtefacts)
	readableBoards = func(_ context.Context, ids []string, _ string) (map[string]bool, error) {
		return map[string]bool{ids[0]: true}, nil
	}
	ana := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "ana-uuid"}}

	for _, c := range []struct {
		name string
		// rows are the board's calls, the latest shown first; want, the
		// indexes of those listed; pages, how many pages are read.
		rows  []model.BoardCall
		want  []int
		pages int
	}{
		{"30 calls she may not read, then hers", append(boardRows(MaxBoardCalls, now, theirs), boardRows(40, now.Add(-time.Hour), ours)...),
			span(MaxBoardCalls, 2*MaxBoardCalls), 2},
		{"some she may not read among hers", append(append(boardRows(5, now, ours), boardRows(25, now.Add(-10*time.Minute), theirs)...), boardRows(40, now.Add(-time.Hour), ours)...),
			append(span(0, 5), span(30, 55)...), 2},
		{"fewer than MaxBoardCalls she may read in all", append(boardRows(MaxBoardCalls, now, theirs), boardRows(3, now.Add(-time.Hour), ours)...),
			span(MaxBoardCalls, MaxBoardCalls+3), 2},
		{"exactly a page of calls, none hers", boardRows(MaxBoardCalls, now, theirs), nil, 2},
		{"no calls at all", nil, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var starts []model.BoardCallsAfter
			callsOfBoard = boardPages(t, c.rows, &starts)
			reads, asked := 0, map[string]int{}
			mayReadCalls = func(_ context.Context, _ *userModels.UserInfo, refs []Ref) (map[Ref]Reading, error) {
				reads++
				out := map[Ref]Reading{}
				for _, ref := range refs {
					if asked[ref.ID]++; ref.ID == mine {
						out[ref] = Reading{}
					}
				}
				return out, nil
			}
			artefactReads, windows := 0, 0
			callArtefacts = func(_ context.Context, w []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error) {
				artefactReads++
				windows = len(w)
				return map[string][]recordingDomain.CallArtefact{}, nil
			}

			calls, err := BoardCalls(context.Background(), ana, board.String())
			if err != nil || len(calls) != len(c.want) {
				t.Fatalf("Ana's board calls: %d, %v; want %d", len(calls), err, len(c.want))
			}
			for i, at := range c.want {
				if calls[i].SessionID != c.rows[at].SessionID.String() || calls[i].Conversation != mine || calls[i].Artefacts == nil {
					t.Fatalf("call %d is %+v; want row %d, the latest she may read first", i, calls[i], at)
				}
			}
			if len(starts) != c.pages || starts[0] != (model.BoardCallsAfter{}) {
				t.Fatalf("pages read from %+v; want %d, the first from the top", starts, c.pages)
			}
			for p := 1; p < len(starts); p++ {
				if last := c.rows[p*MaxBoardCalls-1]; starts[p].Session != last.SessionID || !starts[p].ShownAt.Equal(last.ShownAt) {
					t.Fatalf("page %d starts after %+v; want after the last page's last call", p, starts[p])
				}
			}
			for id, n := range asked {
				if n != 1 {
					t.Fatalf("conversation %s's rule asked %d times; want once", id, n)
				}
			}
			if reads > c.pages || artefactReads != 1 || windows != len(c.want) {
				t.Fatalf("%d reads of the rules over %d pages, %d reads of what %d windows kept; want a read a page at most and one of what they kept",
					reads, c.pages, artefactReads, windows)
			}
		})
	}

	// A page that fails fails the read: never a list cut short in silence.
	callsOfBoard = func(_ context.Context, _ uuid.UUID, after model.BoardCallsAfter, _ int) ([]model.BoardCall, error) {
		if after.ShownAt.IsZero() {
			return boardRows(MaxBoardCalls, now, theirs), nil
		}
		return nil, errors.New("the database went away")
	}
	if calls, err := BoardCalls(context.Background(), ana, board.String()); err == nil {
		t.Fatalf("a second page that failed: %+v, no error", calls)
	}
}

// span is the indexes from up to but not including to.
func span(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// However many calls a board was shown that its reader may not read, a read
// of its calls stops after maxBoardCallPages pages: a fixed amount of work,
// and the calls further down aren't listed.
func TestABoardsCallsReadABoundedNumberOfPages(t *testing.T) {
	board := uuid.New()
	mine := uuid.NewString()
	now := time.Now()
	rows := append(boardRows(maxBoardCallPages*MaxBoardCalls+40, now, func(int) string { return uuid.NewString() }),
		boardRows(5, now.Add(-30*24*time.Hour), func(int) string { return mine })...)
	defer func(r, c, m, a any) {
		readableBoards = r.(func(context.Context, []string, string) (map[string]bool, error))
		callsOfBoard = c.(func(context.Context, uuid.UUID, model.BoardCallsAfter, int) ([]model.BoardCall, error))
		mayReadCalls = m.(func(context.Context, *userModels.UserInfo, []Ref) (map[Ref]Reading, error))
		callArtefacts = a.(func(context.Context, []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error))
	}(readableBoards, callsOfBoard, mayReadCalls, callArtefacts)
	readableBoards = func(_ context.Context, ids []string, _ string) (map[string]bool, error) {
		return map[string]bool{ids[0]: true}, nil
	}
	var starts []model.BoardCallsAfter
	callsOfBoard = boardPages(t, rows, &starts)
	reads := 0
	mayReadCalls = func(_ context.Context, _ *userModels.UserInfo, refs []Ref) (map[Ref]Reading, error) {
		reads++
		out := map[Ref]Reading{}
		for _, ref := range refs {
			if ref.ID == mine {
				out[ref] = Reading{}
			}
		}
		return out, nil
	}
	callArtefacts = func(context.Context, []recordingDomain.CallWindow) (map[string][]recordingDomain.CallArtefact, error) {
		return map[string][]recordingDomain.CallArtefact{}, nil
	}
	ana := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "ana-uuid"}}

	calls, err := BoardCalls(context.Background(), ana, board.String())
	if err != nil || len(calls) != 0 {
		t.Fatalf("Ana's board calls: %+v, %v; want none within the bound", calls, err)
	}
	if maxBoardCallPages < 1 || len(starts) != maxBoardCallPages || reads != maxBoardCallPages {
		t.Fatalf("%d pages and %d reads of the rules; want the bound, %d", len(starts), reads, maxBoardCallPages)
	}
}
