package business

// A call named where it's linked (airtable-01: a table's relation field
// linking to calls, and any other reader of business/Access ReadableTitles):
// by its topic, or else "Call in #<channel>, <day>" ("Call, <day>" in a DM or
// group), and only to someone who may read its conversation's calls
// (MayReadAll, the Access Resolver's call rule), never to someone let in by
// the invite to a call going on alone, since that leaves its conversation out
// of their reach. An instant meeting's call belongs to no conversation and
// keeps nothing a table could open, so it is never named, as a board's calls
// leave it out (boardCalls.go).
//
// COST. However many calls are asked: one read of their sessions, one access
// decision over their distinct conversations, and one read of the names of
// the channels whose calls have no topic.

import (
	"context"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/pagefill"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func init() {
	accessBusiness.RegisterTitleReader(conversationBusiness.KindCall, callTitles)
}

// Seams for tests.
var (
	sessionsByIDs  = model.SessionsByIDs
	sessionsBefore = model.SessionsBefore
	channelTitles  = accessBusiness.ReadableTitles
)

// callTitles names the calls of ids that reader may read, by lower-case id.
func callTitles(ctx context.Context, ids []string, reader accessBusiness.Subject) (map[string]string, error) {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if p, err := uuid.Parse(id); err == nil {
			parsed = append(parsed, p)
		}
	}
	rows, err := sessionsByIDs(ctx, parsed)
	if err != nil {
		return nil, err
	}
	named, err := nameReadableCalls(ctx, reader, rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(named))
	for _, c := range named {
		out[c.ID] = c.Title
	}
	return out, nil
}

// LinkableCall is a call someone may link to, as the picker lists it.
type LinkableCall struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	StartedAt string `json:"started_at"`
	Kind      string `json:"conversation_kind"`
}

// The picker's bounds: how many calls it lists, and how many pages of calls
// one listing reads to fill them.
const (
	MaxLinkableCalls  = 50
	linkableCallPages = 5
	linkableCallPage  = 100
)

// ErrLinkableCallsRefused is a listing asked through an API token or by a
// bot: the picker is the app's, read as yourself.
var ErrLinkableCallsRefused = errors.New("callstate: calls to link are listed in the app, as yourself")

// LinkableCalls is the calls who may read, newest first, whose title holds
// query (any, for ""), at most limit (MaxLinkableCalls past it): pages of the
// calls of the conversations who is in (model.SessionsBefore), each kept by
// the rule that names them (nameReadableCalls).
func LinkableCalls(ctx context.Context, who *userModels.UserInfo, query string, limit int) ([]LinkableCall, error) {
	if who == nil || helpers.ActsThroughToken(ctx) || who.UserDgraphInfo.IsBot {
		return nil, ErrLinkableCallsRefused
	}
	if limit <= 0 || limit > MaxLinkableCalls {
		limit = MaxLinkableCalls
	}
	query = strings.ToLower(strings.TrimSpace(query))
	reader := accessBusiness.SubjectOf(who)
	readerID, err := uuid.Parse(reader.UserUUID)
	if err != nil {
		// A signed-in member's id is always a uuid: say so, and list nothing.
		helpers.LogWarnWithContext(ctx, "business/CallState/LinkableCalls: the reader's id is not a uuid: %v", err)
		return nil, nil
	}
	read := func(ctx context.Context, after model.SessionsAfter, size int) ([]model.Session, model.SessionsAfter, error) {
		return sessionsBefore(ctx, readerID, after, size)
	}
	keep := func(ctx context.Context, rows []model.Session) ([]LinkableCall, error) {
		named, err := nameReadableCalls(ctx, reader, rows)
		if err != nil {
			return nil, err
		}
		out := make([]LinkableCall, 0, len(named))
		for _, c := range named {
			if query == "" || strings.Contains(strings.ToLower(c.Title), query) {
				out = append(out, c)
			}
		}
		return out, nil
	}
	return pagefill.Fill(ctx, read, keep, linkableCallPage, limit, linkableCallPages)
}

// nameReadableCalls is rows that reader may read, in their order, each with
// its title: one access decision for all their conversations, and one read of
// the names of the channels whose calls have no topic.
func nameReadableCalls(ctx context.Context, reader accessBusiness.Subject, rows []model.Session) ([]LinkableCall, error) {
	if reader.UserUUID == "" || len(rows) == 0 {
		return nil, nil
	}
	if reader.UserUID == "" {
		uid, err := accessBusiness.UIDOf(ctx, reader)
		if err != nil || uid == "" {
			return nil, err
		}
		reader.UserUID = uid
	}
	var refs []Ref
	for _, s := range rows {
		if ref := refOfSession(s); ref.Kind != "" {
			refs = append(refs, ref)
		}
	}
	may, err := mayReadCalls(ctx, reader, refs)
	if err != nil {
		return nil, err
	}
	readable := make([]model.Session, 0, len(rows))
	var untitledIn []accessBusiness.ItemRef
	for _, s := range rows {
		if r, ok := may[refOfSession(s)]; !ok || r.Invitee {
			continue
		}
		readable = append(readable, s)
		if strings.TrimSpace(s.Topic) == "" && s.ResourceKind == conversationBusiness.KindChannel {
			untitledIn = append(untitledIn, accessBusiness.ItemRef{Kind: conversationBusiness.KindChannel, ID: s.ResourceID})
		}
	}
	channels := map[accessBusiness.ItemRef]string{}
	if len(untitledIn) > 0 {
		if channels, err = channelTitles(ctx, []accessBusiness.Subject{reader}, untitledIn); err != nil {
			return nil, err
		}
	}
	out := make([]LinkableCall, 0, len(readable))
	for _, s := range readable {
		title := strings.TrimSpace(s.Topic)
		if title == "" {
			title = "Call, " + s.StartedAt.UTC().Format("2 Jan 2006")
			if name := channels[accessBusiness.ItemRef{Kind: conversationBusiness.KindChannel, ID: s.ResourceID}]; name != "" {
				title = "Call in #" + name + ", " + s.StartedAt.UTC().Format("2 Jan 2006")
			}
		}
		out = append(out, LinkableCall{ID: strings.ToLower(s.ID.String()), Title: title, StartedAt: s.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), Kind: s.ResourceKind})
	}
	return out, nil
}

// refOfSession is the conversation a call belongs to; no kind for an
// instant meeting's, which belongs to none. Pure.
func refOfSession(s model.Session) Ref {
	switch s.ResourceKind {
	case conversationBusiness.KindChannel, conversationBusiness.KindDM, conversationBusiness.KindGroup:
		return Ref{Kind: s.ResourceKind, ID: s.ResourceID}
	}
	return Ref{}
}
