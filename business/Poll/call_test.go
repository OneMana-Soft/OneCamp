package business

import (
	"testing"

	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

func callPoll(anonymous, hidden bool) *pollModel.Poll {
	session := uuid.New()
	return &pollModel.Poll{
		ID: uuid.New(), SessionID: &session, Question: "Which day?", CreatedBy: uuid.New(),
		Options:   []pollModel.Option{{ID: "1", Text: "Tue"}, {ID: "2", Text: "Wed"}},
		Anonymous: anonymous, ResultsHidden: hidden,
	}
}

// Held-back results show no counts to someone who doesn't run the call, only
// their own choice; whoever runs it sees them. A call's poll names no channel
// and no author.
func TestHeldBackResultsAreForWhoRunsTheCall(t *testing.T) {
	p := callPoll(false, true)
	tally := &pollModel.Tally{Counts: map[string]int{"1": 3, "2": 1}, Voters: 4, Mine: []string{"2"}}
	ben := CallView(p, tally, false)
	if ben.Voters != 0 || ben.Options[0].Votes != 0 || ben.Options[1].Votes != 0 || len(ben.Mine) != 1 || !ben.ResultsHidden || ben.CanClose {
		t.Errorf("a participant: %+v", ben)
	}
	maya := CallView(p, tally, true)
	if maya.Voters != 4 || maya.Options[0].Votes != 3 || !maya.CanClose {
		t.Errorf("whoever runs the call: %+v", maya)
	}
	if maya.Channel != "" || maya.CreatedBy != "" || maya.PostID != "" {
		t.Errorf("a call's poll names no channel, author or post: %+v", maya)
	}
	shared := CallView(callPoll(false, false), tally, false)
	if shared.Voters != 4 || shared.Options[0].Votes != 3 {
		t.Errorf("shared results: %+v", shared)
	}
}

// A vote is kept under the member's own uuid, or a guest's made from their
// call identity, both the same every time; in an anonymous poll, under a key
// that is neither, the same for the same person in the same poll only.
func TestWhoAVoteIsKeptUnder(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	ben := uuid.New()
	open, anon, anon2 := callPoll(false, false), callPoll(true, false), callPoll(true, false)
	if got, err := CallVoter(open, "0xben", ben.String()); err != nil || got != ben {
		t.Errorf("a member in an open poll: %v %v", got, err)
	}
	g1, _ := CallVoter(open, "guest-g-1", "")
	g2, _ := CallVoter(open, "guest-g-1", "")
	g3, _ := CallVoter(open, "guest-g-2", "")
	if g1 != g2 || g1 == g3 || g1 == uuid.Nil {
		t.Errorf("guests: %v %v %v", g1, g2, g3)
	}
	a1, err := CallVoter(anon, "0xben", ben.String())
	a2, _ := CallVoter(anon, "0xben", ben.String())
	other, _ := CallVoter(anon2, "0xben", ben.String())
	if err != nil || a1 != a2 || a1 == ben || a1 == other {
		t.Errorf("anonymous: %v %v %v %v", a1, a2, other, err)
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := CallVoter(anon, "0xben", ben.String()); err == nil {
		t.Error("an anonymous vote with no secret to key it")
	}
}
