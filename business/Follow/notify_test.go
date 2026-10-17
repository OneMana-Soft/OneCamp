package business

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

type telling struct {
	notice notificationBusiness.Notice
	to     []string
}

type tellings struct {
	mu  sync.Mutex
	all []telling
	// asked is each question put to the Access Resolver: who it decided for.
	asked [][]string
}

func (ts *tellings) of(kind string) []telling {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []telling
	for _, t := range ts.all {
		if t.notice.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// privateLevels is what the Access Resolver decides on privateDoc for each of
// its people: the owner full, the editor edit, the two readers view, anyone
// else none. A test that reaches someone some other way (a team, a space)
// adds them; one whose grant ended takes them out.
func privateLevels() map[string]accessBusiness.Level {
	return map[string]accessBusiness.Level{ownerID: accessBusiness.LevelFull, editID: accessBusiness.LevelEdit,
		talkID: accessBusiness.LevelView, readID: accessBusiness.LevelView}
}

func withNotifySeams(t *testing.T, follows map[string]string) *tellings {
	return withNotifySeamsAt(t, follows, privateLevels())
}

// withNotifySeamsAt is withNotifySeams with the resolver's answer for each
// person (levels; anyone missing has none).
func withNotifySeamsAt(t *testing.T, follows map[string]string, levels map[string]accessBusiness.Level) *tellings {
	t.Helper()
	ts := &tellings{}
	oldP, oldD, oldF, oldT, oldDecide := people, docPeople, followModes, tell, decide
	t.Cleanup(func() { people, docPeople, followModes, tell, decide = oldP, oldD, oldF, oldT, oldDecide })
	decide = func(_ context.Context, _ string, ids []string) (map[string]accessBusiness.Decision, error) {
		ts.mu.Lock()
		ts.asked = append(ts.asked, ids)
		ts.mu.Unlock()
		out := map[string]accessBusiness.Decision{}
		for _, id := range ids {
			out[id] = accessBusiness.Decision{Level: levels[id], Owner: id == ownerID}
		}
		return out, nil
	}
	people = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, id := range ids {
			if id != "" {
				u := user(id)
				u.UserName = "Person " + id[len(id)-2:]
				out = append(out, u)
			}
		}
		return out, nil
	}
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) { return privateDoc(), nil }
	followModes = func(context.Context, string, string) (map[string]string, error) { return follows, nil }
	tell = func(_ context.Context, n notificationBusiness.Notice, r []notificationBusiness.NoticeRecipient) []string {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		var to []string
		for _, x := range r {
			to = append(to, x.UserUUID+":"+x.Reason)
		}
		ts.all = append(ts.all, telling{notice: n, to: to})
		return nil
	}
	return ts
}

var noon = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// A reader newly mentioned is told, keyed by doc, person and day; someone
// mentioned who can't open the doc isn't, and whoever mentioned them is asked
// to share it.
func TestAMentionTellsReadersAndAsksTheAuthorToShareWithTheRest(t *testing.T) {
	ts := withNotifySeams(t, nil)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocMentioned, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Subjects: []string{readID, awayID}})
	got := ts.of("doc.mentioned")
	if len(got) != 1 || !slices.Equal(got[0].to, []string{readID + ":mention"}) {
		t.Fatalf("mention told %+v", got)
	}
	n := got[0].notice
	if n.DedupKey != "doc.mentioned|"+docID+"|2026-10-12" || n.Email != notificationBusiness.EventDocMention ||
		n.Title != "Person a2 mentioned you in Launch notes" || n.Path != "/app/doc/"+docID {
		t.Errorf("the mention reads %+v", n)
	}
	asks := ts.of(kindMentionUnshared)
	if len(asks) != 1 || !slices.Equal(asks[0].to, []string{editID + ":author"}) {
		t.Fatalf("the author was asked %+v", asks)
	}
	if a := asks[0].notice; a.Data["person"] != awayID || a.Title != "Person a5 can't open Launch notes" || a.Actor != nil {
		t.Errorf("the ask reads %+v", a)
	}
}

// "Akash shared ‘Launch notes’ with you": to the people who gained access,
// with what they may do.
func TestSharingTellsThePeopleWhoGainedAccess(t *testing.T) {
	ts := withNotifySeams(t, nil)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{readID, awayID}, Data: map[string]string{"level": "comment"}})
	got := ts.of("doc.shared")
	if len(got) != 1 || !slices.Equal(got[0].to, []string{readID + ":subject"}) {
		t.Fatalf("sharing told %+v (the doc read back isn't shared with a5 yet)", got)
	}
	if n := got[0].notice; n.Title != "Person a1 shared ‘Launch notes’ with you" || n.Body != "You can read it and comment on it." || n.Data["level"] != "comment" {
		t.Errorf("sharing reads %+v", n)
	}
}

// A new proposal tells the owner (unless they chose "none") and whoever
// follows the doc all; an agent's names the person who asked it.
func TestAProposalTellsTheOwnerAndFollowers(t *testing.T) {
	ts := withNotifySeams(t, map[string]string{readID: ModeAll, talkID: ModeMentions})
	agent := "00000000-0000-4000-8000-0000000000b7"
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID, AgentUUID: agent}, At: noon, Data: map[string]string{"proposal_id": "p1", "author_kind": "agent"}})
	got := ts.of("doc.proposal.created")
	if len(got) != 1 || !slices.Equal(got[0].to, []string{ownerID + ":owner", readID + ":follower"}) {
		t.Fatalf("a proposal told %+v", got)
	}
	if n := got[0].notice; n.Title != "Person b7 proposed a change to Launch notes for Person a2" || n.DedupKey != "doc.proposal.created|p1" {
		t.Errorf("the proposal reads %+v", n)
	}

	ts = withNotifySeams(t, map[string]string{ownerID: ModeNone})
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Data: map[string]string{"proposal_id": "p2"}})
	if got := ts.of("doc.proposal.created"); len(got) != 1 || len(got[0].to) != 0 {
		t.Errorf("an owner who chose none was told: %+v", got)
	}
}

// A decision tells the proposer, or whoever asked the agent that proposed it.
func TestADecisionTellsTheProposer(t *testing.T) {
	for kind, verb := range map[resourceEventBusiness.Kind]string{
		resourceEventBusiness.DocProposalAccepted:  "accepted",
		resourceEventBusiness.DocProposalDeclined:  "declined",
		resourceEventBusiness.DocProposalWithdrawn: "withdrew",
	} {
		ts := withNotifySeams(t, nil)
		notify(context.Background(), resourceEventBusiness.Event{Kind: kind, ResourceKind: "doc", ResourceUUID: docID,
			Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{talkID},
			Data: map[string]string{"proposal_id": "p1", "author_kind": "person", "status": "accepted"}})
		got := ts.of(string(kind))
		if len(got) != 1 || !slices.Equal(got[0].to, []string{talkID + ":subject"}) {
			t.Fatalf("%s told %+v", kind, got)
		}
		if want := "Person a1 " + verb + " your proposed change to Launch notes"; got[0].notice.Title != want {
			t.Errorf("%s reads %q, want %q", kind, got[0].notice.Title, want)
		}
	}
}

// Nothing about a doc in the trash, or on something not a doc.
func TestNobodyIsToldAboutADocInTheTrash(t *testing.T) {
	ts := withNotifySeams(t, nil)
	gone := time.Now().Add(-time.Minute)
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		d := privateDoc()
		d.DeletedAt = &gone
		return d, nil
	}
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{readID}})
	if len(ts.all) != 0 {
		t.Errorf("a doc in the trash told %+v", ts.all)
	}
	if notifies(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "channel"}) ||
		notifies(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocRenamed, ResourceKind: "doc"}) {
		t.Error("the notify sink wants events it has no rule for")
	}
	if !strings.HasPrefix(kindMentionUnshared, "doc.") {
		t.Error("an ask is a doc kind")
	}
}

// A reply in a thread tells its people and the doc's all-followers, once per
// comment; a participant who chose none hears only when mentioned; those whose
// feed lists the comment (the owner, the mentioned) get no second inbox item.
func TestAThreadReplyTellsItsPeopleAndFollowers(t *testing.T) {
	ts := withNotifySeams(t, map[string]string{readID: ModeAll, talkID: ModeNone})
	old := commentTold
	t.Cleanup(func() { commentTold = old })
	commentTold = func(context.Context, string) (string, []string, error) {
		return "<p>Sounds right</p>", []string{ownerID}, nil
	}
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadReplied, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Subjects: []string{ownerID, talkID, awayID},
		Data: map[string]string{"thread_id": "t1", "comment_id": "c9", "status": "open"}})
	got := ts.of("thread.replied")
	if len(got) != 1 {
		t.Fatalf("told %+v", got)
	}
	// talkID chose none and isn't mentioned; awayID can't read the doc.
	if want := []string{ownerID + ":participant", readID + ":follower"}; !slices.Equal(got[0].to, want) {
		t.Errorf("told %v, want %v", got[0].to, want)
	}
	n := got[0].notice
	if n.DedupKey != "comment|c9" || n.Email != notificationBusiness.EventDocComment || n.Body != "Sounds right" ||
		n.Title != "Person a2 replied in a thread on Launch notes" || n.Data["thread_id"] != "t1" {
		t.Errorf("the reply reads %+v", n)
	}
}

// Resolving or reopening a thread tells its people in the app only; a new
// thread is told by its comment, doc-wide, and a re-anchored one tells nobody.
func TestAThreadsStatusIsToldInTheApp(t *testing.T) {
	ts := withNotifySeams(t, nil)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadResolved, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{talkID}, Data: map[string]string{"thread_id": "t1", "status": "resolved"}})
	got := ts.of("thread.resolved")
	if len(got) != 1 || got[0].notice.Title != "Person a1 resolved a thread on Launch notes" || got[0].notice.Email != "" {
		t.Fatalf("resolved told %+v", got)
	}
	for _, k := range []resourceEventBusiness.Kind{resourceEventBusiness.ThreadCreated, resourceEventBusiness.ThreadReanchored} {
		if notifies(resourceEventBusiness.Event{Kind: k, ResourceKind: "doc"}) {
			t.Errorf("%s is told by the notify sink", k)
		}
	}
}
