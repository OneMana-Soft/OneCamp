package business

// What everyone in a call does together (calls package A, "Ask the room":
// meet-10, zoom-02): polls and Q&A, in every kind of call.
//
// Whoever may run them (CanActivities) starts and closes polls, shares
// held-back results, opens Q&A and says which questions people see, and
// marks questions answered, pins or dismisses them. Everyone in the call,
// guests and invitees included, votes, asks and upvotes, only while they're
// in it, and no faster than the limits below. Never an agent, never through
// an API token.
//
// Every screen in the call hears that something changed from a server packet
// that names nobody, then reads its own view again: no packet can name an
// anonymous voter or asker, and a dismissed question leaves every screen at
// once. The packet says which kind of change it was only where a screen with
// the panel shut should say so (told). Anonymous votes and questions are kept so that nobody, the
// database included, can tell whose they are (business/Poll's CallVoter and
// business/CallQA).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	callQABusiness "github.com/akashc777/OneCamp/business/CallQA"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

// What can be asked of a call's activities.
const (
	OpPoll      = "poll"          // poll: start one
	OpClosePoll = "close_poll"    // id: end its voting
	OpShare     = "share_results" // id: show its held-back counts to everyone
	OpVote      = "vote"          // id, options: your choices (none takes your vote back)
	OpQA        = "qa"            // qa: open or close Q&A, which questions people see, anonymous asks
	OpAsk       = "ask"           // text, anonymous
	OpUpvote    = "upvote"        // id, on
	OpQuestion  = "question"      // id, state (open, answered, dismissed) and/or pinned
)

// Activity is one thing asked of a call's activities.
type Activity struct {
	Op        string                 `json:"op"`
	ID        string                 `json:"id,omitempty"`
	Poll      *pollBusiness.CallPoll `json:"poll,omitempty"`
	Options   []string               `json:"options,omitempty"`
	QA        *callStateBusiness.QA  `json:"qa,omitempty"`
	Text      string                 `json:"text,omitempty"`
	Anonymous bool                   `json:"anonymous,omitempty"`
	On        bool                   `json:"on,omitempty"`
	State     string                 `json:"state,omitempty"`
	Pinned    *bool                  `json:"pinned,omitempty"`
}

// What an activities packet says changed, for a screen with the panel shut:
// a poll started, its held-back results shared, Q&A changed, a question
// asked. Never who, and nothing for a vote, an upvote, a close or a question
// marked: the panel, while open, reads those again.
const (
	ToldPoll     = "poll"
	ToldResults  = "results"
	ToldQA       = "qa"
	ToldQuestion = "question"
)

// told is what an operation's packet says changed (the Told names), or "".
// Pure.
func told(op string) string {
	switch op {
	case OpPoll:
		return ToldPoll
	case OpShare:
		return ToldResults
	case OpQA:
		return ToldQA
	case OpAsk:
		return ToldQuestion
	}
	return ""
}

// RunsActivities is whether an operation is for whoever runs the call's
// activities (CanActivities) alone. Pure.
func RunsActivities(op string) bool {
	return op == OpPoll || op == OpClosePoll || op == OpShare || op == OpQA || op == OpQuestion
}

// Activities is a call's polls and questions as one person in it sees them.
type Activities struct {
	Polls     []*pollBusiness.View  `json:"polls"`
	Questions []callQABusiness.View `json:"questions"`
	QA        callStateBusiness.QA  `json:"qa"`
	// Runs is whether the reader runs them.
	Runs bool `json:"runs"`
}

// The most one person does a minute in a call, by call identity, so nobody
// floods the room: votes (changing one is a vote), questions, upvotes.
var (
	voteLimit   = helpers.NewRateLimiter(30, time.Minute)
	askLimit    = helpers.NewRateLimiter(5, time.Minute)
	upvoteLimit = helpers.NewRateLimiter(60, time.Minute)
)

// ActivitiesFor is a call's activities as a member reads them: while they're
// in the call, or once it has ended, if they were in it (a meeting's page,
// which has no post for the report). Never through an API token.
func ActivitiesFor(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID string) (*Activities, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	if actor == nil || actor.Uid == "" {
		return nil, ErrNotInCall
	}
	s, err := callStateBusiness.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNotInCall
	}
	me := s.Person(actor.Uid)
	if s.Live {
		if me, err = hereNow(ctx, s, actor.Uid); err != nil {
			return nil, err
		}
	}
	if me == nil || me.Guest || me.Removed {
		return nil, ErrNotInCall
	}
	return activitiesOf(ctx, s, me)
}

// DoActivity does what a member in a call asks of its activities, and
// answers the id of a poll started or a question asked.
func DoActivity(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID string, a Activity) (string, error) {
	if helpers.ActsThroughToken(ctx) {
		return "", ErrThroughToken
	}
	if actor == nil || actor.Uid == "" {
		return "", ErrNotInCall
	}
	s, me, err := present(ctx, sessionID, actor.Uid)
	if err != nil {
		return "", err
	}
	return do(ctx, s, me, a)
}

// guestIn is the live call a guest's call token is for, and the guest as the
// call has them, while they're in it.
func guestIn(ctx context.Context, token string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
	identity, room, err := GuestOf(token)
	if err != nil {
		return nil, nil, ErrNotInCall
	}
	s, err := guestSession(ctx, room)
	if err != nil {
		return nil, nil, err
	}
	me, err := hereNow(ctx, s, identity)
	if err != nil {
		return nil, nil, err
	}
	if me.Removed {
		return nil, nil, ErrNotInCall
	}
	return s, me, nil
}

// GuestActivities is a call's activities as a guest in it reads them, by
// their call token.
func GuestActivities(ctx context.Context, token string) (*Activities, error) {
	s, me, err := guestIn(ctx, token)
	if err != nil {
		return nil, err
	}
	return activitiesOf(ctx, s, me)
}

// GuestDoesActivity is a guest voting, asking or upvoting, by their call
// token. Nothing the people running a call do is open to a guest.
func GuestDoesActivity(ctx context.Context, token string, a Activity) (string, error) {
	if RunsActivities(a.Op) {
		return "", ErrNotAllowed
	}
	s, me, err := guestIn(ctx, token)
	if err != nil {
		return "", err
	}
	return do(ctx, s, me, a)
}

// runsActivities is whether someone runs a call's activities.
func runsActivities(s *callStateBusiness.Session, p *callStateBusiness.Person) bool {
	return slices.Contains(AbilitiesOf(s.Resource.Kind, p.Role), CanActivities)
}

func activitiesOf(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person) (*Activities, error) {
	session, err := uuid.Parse(s.ID)
	if err != nil {
		return nil, ErrNotInCall
	}
	runs := runsActivities(s, me)
	polls, err := pollBusiness.CallViews(ctx, session, func(p *pollModel.Poll) (uuid.UUID, error) {
		return pollBusiness.CallVoter(p, me.Identity, me.UserUUID)
	}, runs)
	if err != nil {
		return nil, err
	}
	questions, err := callQABusiness.Views(ctx, session, me.Identity, runs, s.QA.Shown)
	if err != nil {
		return nil, err
	}
	return &Activities{Polls: polls, Questions: questions, QA: s.QA, Runs: runs}, nil
}

// do does one thing asked of a call's activities by someone in it, then tells
// every screen in the call to read its view again.
func do(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person, a Activity) (string, error) {
	if RunsActivities(a.Op) && !runsActivities(s, me) {
		return "", ErrNotAllowed
	}
	session, err := uuid.Parse(s.ID)
	if err != nil {
		return "", ErrNotInCall
	}
	limit := func(l *helpers.RateLimiter) bool { return l.Allow(s.ID + " " + me.Identity) }
	id := ""
	switch a.Op {
	case OpPoll:
		if a.Poll == nil {
			return "", badActivity("Say what the poll asks.")
		}
		creator, perr := uuid.Parse(me.UserUUID)
		if perr != nil {
			return "", ErrNotAllowed
		}
		var p *pollModel.Poll
		if p, err = pollBusiness.CreateInCall(ctx, session, creator, *a.Poll); err == nil {
			id = p.ID.String()
		}
	case OpClosePoll, OpShare, OpVote:
		if a.Op == OpVote && !limit(voteLimit) {
			return "", ErrTooFast
		}
		err = onPoll(ctx, session, me, a)
	case OpQA:
		if a.QA == nil {
			return "", badActivity("Say how questions should work in this call.")
		}
		qa := *a.QA
		if qa.Shown != callQABusiness.ShownAnswered {
			qa.Shown = callQABusiness.ShownAll
		}
		err = callStateBusiness.SetQA(ctx, s, qa)
	case OpAsk:
		if !s.QA.Open {
			return "", ErrQAClosed
		}
		if a.Anonymous && !s.QA.Anonymous {
			return "", ErrNoAnonymous
		}
		if !limit(askLimit) {
			return "", ErrTooFast
		}
		q, qerr := callQABusiness.Ask(ctx, session, me.Identity, me.Name, a.Text, a.Anonymous)
		if err = qerr; err == nil {
			id = q.ID.String()
		}
	case OpUpvote:
		if !limit(upvoteLimit) {
			return "", ErrTooFast
		}
		err = callQABusiness.Upvote(ctx, session, a.ID, me.Identity, a.On)
	case OpQuestion:
		err = callQABusiness.Moderate(ctx, session, a.ID, a.State, a.Pinned)
	default:
		return "", ErrUnknownAction
	}
	if err != nil {
		return "", activityRefusal(err)
	}
	tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "activities", What: told(a.Op)})
	return id, nil
}

// onPoll closes a call's poll, shares its results, or votes in it.
func onPoll(ctx context.Context, session uuid.UUID, me *callStateBusiness.Person, a Activity) error {
	p, err := pollBusiness.InCall(ctx, session, a.ID)
	if err != nil {
		return err
	}
	switch a.Op {
	case OpClosePoll:
		return pollBusiness.CloseInCall(ctx, p)
	case OpShare:
		return pollBusiness.ShareResults(ctx, p)
	}
	voter, err := pollBusiness.CallVoter(p, me.Identity, me.UserUUID)
	if err != nil {
		return err
	}
	return pollBusiness.VoteInCall(ctx, p, voter, a.Options)
}

// badActivity is a request for a call's activities the person can fix.
func badActivity(msg string) *Refusal {
	return &Refusal{http.StatusBadRequest, "bad_activity", msg}
}

// activityRefusal says why something asked of a call's activities wasn't
// done, in the polls' and questions' own words where the person can fix it.
func activityRefusal(err error) error {
	var poll pollBusiness.InputError
	if errors.As(err, &poll) {
		return badActivity(sentence(string(poll)))
	}
	var question callQABusiness.InputError
	if errors.As(err, &question) {
		return badActivity(string(question))
	}
	if errors.Is(err, pollBusiness.ErrNoAccess) || errors.Is(err, callQABusiness.ErrNotFound) {
		return ErrNoActivity
	}
	return err
}

// sentence is business/Poll's words, which read inside a sentence, as one on
// its own: a capital first and a full stop last. Pure.
func sentence(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		s = string(s[0]-'a'+'A') + s[1:]
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
