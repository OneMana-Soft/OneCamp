package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// answering is an Access Resolver that answers each resource by answer.
type answering struct {
	accessBusiness.Resolver
	answer func(accessBusiness.Resource) (accessBusiness.Decision, error)
}

func (a answering) Resolve(_ context.Context, _ accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	return a.answer(res)
}

// MayReadAll asks the calls of many conversations at once, each once, as
// MayRead asks one: a conversation the reader can open, one they were only let
// into by a call's link (an invitee), and one they can't (left out); an
// instant meeting is never asked, its host's rule being MayRead's.
func TestMayReadAllAsksEveryConversationsCallsOnce(t *testing.T) {
	was := accessBusiness.Rules
	defer func() { accessBusiness.Rules = was }()
	open := Ref{Kind: conversationBusiness.KindChannel, ID: uuid.NewString()}
	invited := conversationBusiness.ChatRef(uuid.NewString() + " " + uuid.NewString())
	closed := Ref{Kind: conversationBusiness.KindChannel, ID: uuid.NewString()}
	meeting := Ref{Kind: KindMeeting, ID: uuid.NewString()}
	asked := map[accessBusiness.Resource]int{}
	accessBusiness.Rules = answering{answer: func(res accessBusiness.Resource) (accessBusiness.Decision, error) {
		asked[res]++
		switch res {
		case callConversation(open).Resource():
			return accessBusiness.Decision{Level: conversationBusiness.LevelPost}, nil
		case callConversation(invited).Resource():
			return accessBusiness.Decision{Level: conversationBusiness.LevelRead, Reasons: []string{conversationBusiness.ReasonCallInvite}}, nil
		}
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}}
	me := &userModels.UserInfo{}
	me.UserDgraphInfo.Uid, me.UserDgraphInfo.Uuid = "0x1", "aaaa"

	got, err := MayReadAll(context.Background(), me, []Ref{open, invited, closed, meeting, open})
	if err != nil || len(got) != 2 {
		t.Fatalf("MayReadAll = %+v, %v; want the open conversation and the invited one", got, err)
	}
	if r, ok := got[open]; !ok || r.Invitee {
		t.Fatalf("the open conversation: %+v, %v", r, ok)
	}
	if r, ok := got[invited]; !ok || !r.Invitee {
		t.Fatalf("the conversation let into by a call's link: %+v, %v", r, ok)
	}
	if len(asked) != 3 || asked[callConversation(open).Resource()] != 1 || asked[callConversation(meeting).Resource()] != 0 {
		t.Fatalf("asked %v; want each conversation once and never the meeting", asked)
	}
	for _, ref := range []Ref{open, invited, closed} {
		allowed, invitee, err := MayRead(context.Background(), me, ref)
		if r, ok := got[ref]; err != nil || allowed != ok || invitee != r.Invitee {
			t.Fatalf("MayRead(%v) = %v, %v, %v; MayReadAll says %+v, %v", ref, allowed, invitee, err, r, ok)
		}
	}
	if none, err := MayReadAll(context.Background(), nil, []Ref{open}); err != nil || len(none) != 0 {
		t.Fatalf("nobody: %+v, %v", none, err)
	}
}
