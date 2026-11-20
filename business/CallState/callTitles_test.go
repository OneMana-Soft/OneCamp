package business

import (
	"context"
	"fmt"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// A page of 60 calls across three conversations, an instant meeting's call
// and one Maya was only let into by its invite is named in one read of the
// sessions, one access decision over the three conversations and one read of
// channel names, never a read per call (the 7th review check).
func TestCallTitlesTakeAConstantNumberOfReads(t *testing.T) {
	general, launch, dm := uuid.NewString(), uuid.NewString(), "a b"
	invited := uuid.NewString()
	day := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	var rows []model.Session
	var ids []string
	add := func(kind, resource, topic string) {
		s := model.Session{ID: uuid.New(), ResourceKind: kind, ResourceID: resource, Topic: topic, StartedAt: day}
		rows = append(rows, s)
		ids = append(ids, s.ID.String())
	}
	for i := 0; i < 60; i++ {
		switch i % 3 {
		case 0:
			add("channel", general, "")
		case 1:
			add("channel", launch, fmt.Sprintf("Launch %d", i))
		default:
			add("dm", dm, "")
		}
	}
	add(KindMeeting, uuid.NewString(), "Vendor call")
	add("channel", invited, "Board sync")

	reads, decisions, names := 0, 0, 0
	savedS, savedM, savedC := sessionsByIDs, mayReadCalls, channelTitles
	t.Cleanup(func() { sessionsByIDs, mayReadCalls, channelTitles = savedS, savedM, savedC })
	sessionsByIDs = func(_ context.Context, asked []uuid.UUID) ([]model.Session, error) {
		reads++
		if len(asked) != len(rows) {
			t.Errorf("read %d sessions, want %d", len(asked), len(rows))
		}
		return rows, nil
	}
	mayReadCalls = func(_ context.Context, who accessBusiness.Subject, refs []Ref) (map[Ref]Reading, error) {
		decisions++
		distinct := map[Ref]bool{}
		for _, r := range refs {
			if r.Kind == KindMeeting {
				t.Errorf("an instant meeting's call was asked about: %v", r)
			}
			distinct[r] = true
		}
		if len(distinct) != 4 || who.UserUID != "0x1" {
			t.Errorf("asked %d conversations for %+v", len(distinct), who)
		}
		return map[Ref]Reading{{Kind: "channel", ID: general}: {}, {Kind: "channel", ID: launch}: {}, {Kind: "dm", ID: dm}: {},
			{Kind: "channel", ID: invited}: {Invitee: true}}, nil
	}
	channelTitles = func(_ context.Context, _ []accessBusiness.Subject, refs []accessBusiness.ItemRef) (map[accessBusiness.ItemRef]string, error) {
		names++
		out := map[accessBusiness.ItemRef]string{}
		for _, r := range refs {
			if r.ID == general {
				out[r] = "general"
			}
		}
		return out, nil
	}

	got, err := callTitles(context.Background(), ids, accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || decisions != 1 || names != 1 {
		t.Errorf("reads %d, decisions %d, channel name reads %d; want one each", reads, decisions, names)
	}
	if len(got) != 60 {
		t.Errorf("named %d calls, want the 60 in conversations Maya may read", len(got))
	}
	for i, want := range []string{"Call in #general, 12 Nov 2026", "Launch 1", "Call, 12 Nov 2026"} {
		if got[ids[i]] != want {
			t.Errorf("call %d is named %q, want %q", i, got[ids[i]], want)
		}
	}
	for _, i := range []int{60, 61} {
		if title, ok := got[ids[i]]; ok {
			t.Errorf("call %d (an instant meeting's, or one only invited to) is named %q", i, title)
		}
	}

	if none, err := callTitles(context.Background(), ids, accessBusiness.Subject{}); err != nil || len(none) != 0 {
		t.Errorf("no one reads %v, %v", none, err)
	}
}

// An agent reading a table for its person asks the call rule as that agent,
// never as the person: the subject reaches MayReadAll whole, so a rule that
// tells them apart (an invite lets a person in, never their agent) can
// (CR-374 NB2).
func TestCallTitlesAskTheCallRuleAsTheAgent(t *testing.T) {
	room := uuid.NewString()
	row := model.Session{ID: uuid.New(), ResourceKind: "channel", ResourceID: room, Topic: "Board sync"}
	savedS, savedM := sessionsByIDs, mayReadCalls
	t.Cleanup(func() { sessionsByIDs, mayReadCalls = savedS, savedM })
	sessionsByIDs = func(context.Context, []uuid.UUID) ([]model.Session, error) { return []model.Session{row}, nil }
	var asked []accessBusiness.Subject
	mayReadCalls = func(_ context.Context, who accessBusiness.Subject, refs []Ref) (map[Ref]Reading, error) {
		asked = append(asked, who)
		return map[Ref]Reading{refs[0]: {}}, nil
	}
	agent := accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString(), AgentUUID: uuid.NewString()}
	got, err := callTitles(context.Background(), []string{row.ID.String()}, agent)
	if err != nil || got[row.ID.String()] != "Board sync" {
		t.Fatalf("callTitles = %v, %v; want the call named", got, err)
	}
	if len(asked) != 1 || asked[0] != agent {
		t.Fatalf("the call rule was asked as %+v; want the agent's subject %+v", asked, agent)
	}
}
