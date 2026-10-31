package business

// A call's polls (calls package A, "Ask the room": meet-10, zoom-02): a
// question everyone in a call may answer, asked by whoever runs it, in every
// kind of call, guests included. Stored as every poll is (Normalize's caps,
// poll_votes, the one tally and View), with the call's session and neither a
// channel nor a post: chat's routes never reach one (load refuses it), and who
// may see, vote or close it is the call's to decide (business/CallControl).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

// MaxCallPolls is how many polls one call may have.
const MaxCallPolls = 50

// CallPoll is what whoever runs a call asks.
type CallPoll struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Multiple bool     `json:"multiple"`
	// Anonymous: nobody, whoever runs the call and the database included, can
	// tell whose its votes are.
	Anonymous bool `json:"anonymous"`
	// HideResults keeps the counts for whoever runs the call until they share them.
	HideResults bool `json:"hide_results"`
}

// callVoterSpace names the uuids a call's poll keeps its voters under.
var callVoterSpace = uuid.MustParse("6f1c3a52-9d0e-4b7a-8c1f-2e5d7b9a4c10")

// CreateInCall stores a poll in a call, asked by createdBy (a member who runs
// it): no post, since the call's screens show it.
func CreateInCall(ctx context.Context, session, createdBy uuid.UUID, in CallPoll) (*pollModel.Poll, error) {
	n, err := NewPoll{Question: in.Question, Options: in.Options, Multiple: in.Multiple}.Normalize()
	if err != nil {
		return nil, err
	}
	p := &pollModel.Poll{
		ID: uuid.New(), ChannelID: uuid.Nil, SessionID: &session, Question: n.Question, Multiple: n.Multiple,
		CreatedBy: createdBy, Anonymous: in.Anonymous, ResultsHidden: in.HideResults,
	}
	for i, o := range n.Options {
		p.Options = append(p.Options, pollModel.Option{ID: strconv.Itoa(i + 1), Text: o})
	}
	made, err := pollModel.CreateInSession(ctx, p, MaxCallPolls)
	if err != nil {
		return nil, fmt.Errorf("store poll: %w", err)
	}
	if !made {
		return nil, InputError(fmt.Sprintf("a call can have at most %d polls", MaxCallPolls))
	}
	return p, nil
}

// InCall loads one of a call's polls; any other poll is ErrNoAccess.
func InCall(ctx context.Context, session uuid.UUID, pollID string) (*pollModel.Poll, error) {
	id, err := uuid.Parse(pollID)
	if err != nil {
		return nil, ErrNoAccess
	}
	p, err := pollModel.Get(ctx, id)
	if errors.Is(err, pollModel.ErrNotFound) {
		return nil, ErrNoAccess
	}
	if err != nil {
		return nil, err
	}
	if p.SessionID == nil || *p.SessionID != session {
		return nil, ErrNoAccess
	}
	return p, nil
}

// CallVoter is who someone's vote in a call's poll is kept under: a member's
// own uuid, a guest's made from their call identity, or in an anonymous poll a
// key only this server can make again from who they are
// (helpers.AnonymousKey), so nobody reading the votes can tell whose they are.
func CallVoter(p *pollModel.Poll, identity, userUUID string) (uuid.UUID, error) {
	if p.Anonymous {
		key, err := helpers.AnonymousKey("poll:"+p.ID.String(), identity)
		if err != nil {
			return uuid.Nil, err
		}
		return uuid.NewSHA1(callVoterSpace, key), nil
	}
	if id, err := uuid.Parse(userUUID); err == nil {
		return id, nil
	}
	return uuid.NewSHA1(callVoterSpace, []byte(identity)), nil
}

// VoteInCall sets a voter's choices in a call's poll; none takes the vote back.
func VoteInCall(ctx context.Context, p *pollModel.Poll, voter uuid.UUID, optionIDs []string) error {
	choice, err := ValidateChoice(p, optionIDs, time.Now())
	if err != nil {
		return err
	}
	return pollModel.ReplaceVotes(ctx, p.ID, voter, choice)
}

// CloseInCall ends voting in a call's poll.
func CloseInCall(ctx context.Context, p *pollModel.Poll) error {
	return pollModel.Close(ctx, p.ID)
}

// ShareResults shows a call's poll's counts to everyone in the call.
func ShareResults(ctx context.Context, p *pollModel.Poll) error {
	return pollModel.SetResultsHidden(ctx, p.ID, false)
}

// CallView is a call's poll as one reader sees it: no channel and no author,
// and while its results are held back, no counts for someone who doesn't run
// the call. A reader's own choices are theirs to see. Pure.
func CallView(p *pollModel.Poll, t *pollModel.Tally, runs bool) *View {
	v := view(p, t, uuid.Nil, runs)
	v.CreatedBy = ""
	v.Anonymous, v.ResultsHidden = p.Anonymous, p.ResultsHidden
	if p.ResultsHidden && !runs {
		v.Voters = 0
		for i := range v.Options {
			v.Options[i].Votes = 0
		}
	}
	return v
}

// CallViews lists a call's polls, oldest first, as one reader sees them:
// voter says who their own votes are kept under in a poll (CallVoter).
func CallViews(ctx context.Context, session uuid.UUID, voter func(*pollModel.Poll) (uuid.UUID, error), runs bool) ([]*View, error) {
	polls, err := pollModel.OfSession(ctx, session)
	if err != nil {
		return nil, err
	}
	mine := make(map[uuid.UUID]uuid.UUID, len(polls))
	for _, p := range polls {
		if mine[p.ID], err = voter(p); err != nil {
			return nil, err
		}
	}
	tallies, err := pollModel.CountMany(ctx, mine)
	if err != nil {
		return nil, err
	}
	out := make([]*View, 0, len(polls))
	for _, p := range polls {
		out = append(out, CallView(p, tallies[p.ID], runs))
	}
	return out, nil
}
