package business

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// zoom-13: who finds the words said in a call. A line is found only by
// someone who is a member, now, of the channel or chat the call was in, read
// from the graph when the search runs (never the sign-in's cached lists or
// the index entry), and only once the call has ended. While a call runs
// nobody finds its lines; if the running-calls or membership read fails
// nobody finds any transcript, and every other kind is unaffected. Each page
// is checked against the graph and fails closed. An AI agent's token finds a
// line only where the agent itself is a member of the conversation now, and
// only while BoundAgent lets it act.

// repairTranscript is a transcript's recording, whose entries are marked
// again from the graph.
const repairTranscript = "transcript"

// openCallsMost is the most calls going on a search reads; a full read is a
// failed one (some call would go unseen), so nobody finds a transcript.
const openCallsMost = 500

var errTooManyOpenCalls = errors.New("search: more calls going on than a search reads")

// Seams for tests.
var (
	callPlacesOf      = recordingDomain.CallPlaces
	openCallsOf       = openCalls
	openSessionsOf    = callStateModel.OpenSessions
	transcriptHomesOf = recordingDomain.TranscriptHomes
	// agentMayAct is whether the agent may act now: on the AI edition,
	// BoundAgent's answer; this edition has no agents, so none may.
	agentMayAct = func(ctx context.Context, agentID uuid.UUID) bool { return false }
	// agentAccountOf is the uuid of the agent's own account (read, never
	// made here): its memberships are the agent's.
	agentAccountOf = func(ctx context.Context, agentID uuid.UUID) (string, error) {
		id, err := userDomain.GetUserIDByEmail(ctx, userDomain.AgentBotEmail(agentID))
		if err != nil || id == uuid.Nil {
			return "", errors.New("search: the agent has no account of its own")
		}
		return id.String(), nil
	}
)

// openCalls is every call going on: their session keys and rooms, and for
// each conversation one is in, when the oldest there started (epoch seconds).
func openCalls(ctx context.Context) (CallReach, error) {
	sessions, err := openSessionsOf(ctx, openCallsMost)
	if err != nil {
		return CallReach{}, err
	}
	if len(sessions) >= openCallsMost {
		return CallReach{}, errTooManyOpenCalls
	}
	return runningOf(sessions), nil
}

// runningOf is what keeps the lines of the calls going on out of a search:
// their session keys, their rooms, and each conversation one is in (its room,
// a channel's uuid or a chat's grouping id, and the call's resource) with
// when the oldest there started. Pure.
func runningOf(sessions []callStateModel.Session) CallReach {
	running := CallReach{RunningSince: map[string]int64{}}
	for _, s := range sessions {
		if key := helpers.CallSessionKeyFor(s.RoomSID); key != "" {
			running.RunningKeys = append(running.RunningKeys, key)
		}
		if s.Room != "" {
			running.RunningRooms = append(running.RunningRooms, s.Room)
		}
		since := s.StartedAt.Unix()
		for _, conv := range []string{s.Room, s.ResourceID} {
			if at, ok := running.RunningSince[conv]; conv != "" && (!ok || since < at) {
				running.RunningSince[conv] = since
			}
		}
	}
	return running
}

// actingAgent is the AI agent a search runs as, if any: the subject's or the
// request token's (on the AI edition, an agent run's too). ok is false for an agent id that
// isn't one (nobody to check, so no transcript is found).
func actingAgent(ctx context.Context, who accessBusiness.Subject) (agent uuid.UUID, ok bool) {
	id := who.AgentUUID
	if id == "" {
		id = helpers.GetApiAgentID(ctx)
	}
	if id == "" {
		return uuid.Nil, true
	}
	agent, err := uuid.Parse(id)
	return agent, err == nil && agent != uuid.Nil
}

// AgentCallPlaces is, when an AI agent acts (actingAgent: who's, the request
// token's or the agent run's), the conversations its own account is in now
// (CallPlaces), with acting true: a call's transcript is the agent's to read
// only in those its sponsor is in too (V-1100), for a search and an export
// alike. For a person's own request, acting is false and nothing is read. An
// agent id that isn't one, or one BoundAgent doesn't let act, is an error.
func AgentCallPlaces(ctx context.Context, who accessBusiness.Subject) (places []string, acting bool, err error) {
	agent, ok := actingAgent(ctx, who)
	if !ok {
		return nil, true, errors.New("search: the acting agent isn't one")
	}
	if agent == uuid.Nil {
		return nil, false, nil
	}
	if !agentMayAct(ctx, agent) {
		return nil, true, errors.New("search: the acting agent may not act")
	}
	account, err := agentAccountOf(ctx, agent)
	if err != nil {
		return nil, true, err
	}
	places, err = callPlacesOf(ctx, account)
	return places, true, err
}

// callReach reads, when a search looks at transcripts, the conversations the
// asker is in now and every call going on. For an AI agent, the places are
// those both its sponsor and its own account are in, and none unless
// BoundAgent lets it act. An error finds no transcript.
func callReach(ctx context.Context, r Reach) (*CallReach, error) {
	own, acting, err := AgentCallPlaces(ctx, r.Who)
	if err != nil {
		return nil, err
	}
	places, err := callPlacesOf(ctx, r.UserUUID)
	if err != nil {
		return nil, err
	}
	if acting {
		places = bothIn(places, own)
	}
	running, err := openCallsOf(ctx)
	if err != nil {
		return nil, err
	}
	running.Places = places
	return &running, nil
}

// bothIn is the items of a that b holds too, in a's order.
func bothIn(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

// running is whether a line is of a call going on, by reach: its call's
// session key or (for a line with none) its room, or said in a conversation
// since the call going on there started.
func (c *CallReach) running(t *openSearchStruct.OpenSearchTranscript) bool {
	if t.SessionKey != "" && contains(c.RunningKeys, t.SessionKey) {
		return true
	}
	if t.SessionKey == "" && t.Room != "" && contains(c.RunningRooms, t.Room) {
		return true
	}
	since, ok := c.RunningSince[t.ConvID]
	return ok && t.CreatedAt >= since
}

// keepReadableTranscriptHits is a page less the transcript lines the graph
// says the asker can't find now, in one read for the page. A line whose
// recording is gone or no longer holds it is forgotten; a deleted
// recording's entries are marked again from the graph; a line of a call not
// yet ended, of a call going on, or of a conversation the asker isn't in now
// is left out. A check that can't be made leaves every line out and every
// other hit in.
func keepReadableTranscriptHits(ctx context.Context, reach Reach, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	var keys, lines []string
	for _, h := range hits {
		if t := transcriptOfHit(h); t != nil {
			keys = append(keys, t.RecordingID)
			lines = append(lines, t.Uuid)
		}
	}
	if len(keys) == 0 {
		return hits
	}
	var homes map[string]recordingDomain.TranscriptHome
	var places map[string]bool
	if reach.Calls == nil {
		helpers.LogErrorWithContext(ctx, "search: no call reach to check %d transcript lines by, leaving them out", len(lines))
	} else if read, err := transcriptHomesOf(ctx, keys, lines); err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not check %d transcript lines against the graph, leaving them out", len(lines))
	} else {
		homes, places = read, make(map[string]bool, len(reach.Calls.Places))
		for _, p := range reach.Calls.Places {
			places[p] = true
		}
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	var gone, deleted []*openSearchStruct.GlobalSearchOpenSearchResp
	dropped := 0
	for _, h := range hits {
		t := transcriptOfHit(h)
		if t == nil {
			kept = append(kept, h)
			continue
		}
		if homes == nil {
			dropped++
			continue
		}
		home, ok := homes[t.RecordingID]
		switch {
		case !ok || !home.Lines[t.Uuid]:
			gone = append(gone, h)
		case home.Deleted:
			deleted = append(deleted, h)
		case !home.Ended, home.ConvID == "", t.ConvID != home.ConvID, !places[home.ConvID], reach.Calls.running(t):
			dropped++
		default:
			kept = append(kept, h)
		}
	}
	if len(gone) > 0 {
		helpers.LogInfoWithContext(ctx, "search: dropped %d transcript lines whose recording no longer holds them", len(gone))
		forgetFn(gone)
	}
	if len(deleted) > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d lines of deleted recordings", len(deleted))
		repairDropped(droppedKind{repairTranscript, droppedIDs(deleted, nil, func(h *openSearchStruct.GlobalSearchOpenSearchResp) string {
			if t := transcriptOfHit(h); t != nil {
				return t.RecordingID
			}
			return ""
		})})
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d transcript lines the person can't find now", dropped)
	}
	return kept
}

// transcriptOfHit is the line a hit carries; nil for a hit that carries none.
func transcriptOfHit(h *openSearchStruct.GlobalSearchOpenSearchResp) *openSearchStruct.OpenSearchTranscript {
	if h == nil {
		return nil
	}
	return h.Transcript
}
