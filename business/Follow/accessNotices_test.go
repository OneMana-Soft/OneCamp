package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
)

// teamID is someone privateDoc's lists don't name, whom the resolver lets
// read it through a team (or the doc's space).
const teamID = "00000000-0000-4000-8000-0000000000a6"

// Who may read the doc is the Access Resolver's answer, asked once an event,
// not the doc's lists (A5×A6): someone it reaches through a team is told of a
// mention; someone on its lists whose grant has ended isn't, and whoever
// mentioned them is asked to share it; and when the resolver can't answer,
// nobody is told and nobody is asked.
func TestTheRulesTellWhomTheResolverLetsRead(t *testing.T) {
	levels := privateLevels()
	levels[teamID] = accessBusiness.LevelView
	delete(levels, readID) // on the doc's reading list, but their grant ended
	ts := withNotifySeamsAt(t, nil, levels)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocMentioned, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Subjects: []string{teamID, readID}})
	if got := ts.of("doc.mentioned"); len(got) != 1 || !slices.Equal(got[0].to, []string{teamID + ":mention"}) {
		t.Errorf("the mention told %+v, want the team's member alone", got)
	}
	if asks := ts.of(kindMentionUnshared); len(asks) != 1 || asks[0].notice.Data["person"] != readID {
		t.Errorf("the author was asked about %+v, want the person whose grant ended", asks)
	}
	if len(ts.asked) != 1 || !slices.Equal(ts.asked[0], []string{teamID, readID}) {
		t.Errorf("the resolver was asked %v, want once about the two mentioned", ts.asked)
	}

	// A follower reached through a team hears of a proposal; one whose grant
	// ended doesn't.
	ts = withNotifySeamsAt(t, map[string]string{teamID: ModeAll, readID: ModeAll}, levels)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Data: map[string]string{"proposal_id": "p1"}})
	if got := ts.of("doc.proposal.created"); len(got) != 1 || !slices.Equal(got[0].to, []string{ownerID + ":owner", teamID + ":follower"}) {
		t.Errorf("a proposal told %+v", got)
	}

	ts = withNotifySeams(t, nil)
	decide = func(context.Context, string, []string) (map[string]accessBusiness.Decision, error) {
		return nil, errors.New("graph down")
	}
	for _, k := range []resourceEventBusiness.Kind{resourceEventBusiness.DocMentioned, resourceEventBusiness.DocShared, resourceEventBusiness.ThreadResolved} {
		notify(context.Background(), resourceEventBusiness.Event{Kind: k, ResourceKind: "doc", ResourceUUID: docID,
			Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Subjects: []string{readID, awayID},
			Data: map[string]string{"thread_id": "t1", "level": "view"}})
	}
	if len(ts.all) != 0 {
		t.Errorf("with the resolver down, told %+v", ts.all)
	}
}

// The doc.shared that approving a request makes is told by the answer, so
// its asker hears once.
func TestAShareThatAnswersARequestIsLeftToTheAnswer(t *testing.T) {
	ts := withNotifySeams(t, nil)
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{readID},
		Data: map[string]string{"level": "edit", "request": "r1"}})
	if len(ts.all) != 0 || len(ts.asked) != 0 {
		t.Errorf("a share answering a request told %+v", ts.all)
	}
}

func withRequest(t *testing.T, req *accessModels.Request, err error) {
	t.Helper()
	old := requestOf
	t.Cleanup(func() { requestOf = old })
	requestOf = func(context.Context, string) (*accessModels.Request, error) { return req, err }
}

// A request for access reaches the people who can answer it, the doc's owner
// and its full-access people, by the resolver now: never an editor, never the
// asker. It names the doc (they can read it) and carries the asker's note.
func TestARequestForAccessReachesWhoCanAnswerIt(t *testing.T) {
	levels := privateLevels()
	levels[teamID] = accessBusiness.LevelFull // given full access by name
	ts := withNotifySeamsAt(t, nil, levels)
	withRequest(t, &accessModels.Request{Status: accessModels.StatusPending, Note: "  For the launch review  "}, nil)
	ask := resourceEventBusiness.Event{Kind: resourceEventBusiness.DocAccessRequested, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: awayID}, At: noon, Subjects: []string{ownerID, teamID, editID},
		Data: map[string]string{"request": "r1", "level": "edit"}}
	if !notifies(ask) || !isAudited(ask) {
		t.Fatal("a request for access is neither told nor audited")
	}
	notify(context.Background(), ask)
	got := ts.of("doc.access_requested")
	if len(got) != 1 || !slices.Equal(got[0].to, []string{ownerID + ":subject", teamID + ":subject"}) {
		t.Fatalf("the request told %+v, want the owner and the full-access person", got)
	}
	n := got[0].notice
	if n.Title != "Person a5 asked to edit Launch notes" || n.Body != "“For the launch review” Approve or decline it from Share, on the page." ||
		n.DedupKey != "doc.access_requested|r1|edit" || n.Email != notificationBusiness.EventDocShared || n.Path != "/app/doc/"+docID ||
		n.Data["request"] != "r1" || n.Data["level"] != "edit" || len(n.Data) != 2 || n.Actor == nil || n.Actor.Uuid != awayID {
		t.Errorf("the request reads %+v", n)
	}

	// Answered already: nobody. Unreadable: told, without the note.
	ts = withNotifySeamsAt(t, nil, levels)
	withRequest(t, &accessModels.Request{Status: accessModels.StatusApproved}, nil)
	notify(context.Background(), ask)
	if len(ts.all) != 0 {
		t.Errorf("a request answered already told %+v", ts.all)
	}
	ts = withNotifySeamsAt(t, nil, levels)
	withRequest(t, nil, errors.New("postgres down"))
	notify(context.Background(), ask)
	if got := ts.of("doc.access_requested"); len(got) != 1 || got[0].notice.Body != "Approve or decline it from Share, on the page." {
		t.Errorf("with the request unreadable, told %+v", got)
	}
}

// revealsNothing says what a notice to someone who can't open the doc shows
// of it, if anything: they asked from a page that named nothing of the doc
// (B6's NoAccessPage), so the notice may say only what became of their
// request, at the level they asked for, on the doc whose link they had.
func revealsNothing(t *testing.T, n notificationBusiness.Notice) {
	t.Helper()
	for _, s := range []string{n.Title, n.Body, n.ResourceTitle} {
		if strings.Contains(s, "Launch notes") || strings.Contains(s, "Person a1") {
			t.Errorf("the asker's notice names the doc or who answered: %q", s)
		}
	}
	for k, v := range n.Data {
		if k != "request" && k != "status" && k != "level" {
			t.Errorf("the asker's notice carries %s=%q", k, v)
		}
	}
	if n.Actor != nil || n.Agent != nil || !n.Closed || n.ResourceTitle != "" {
		t.Errorf("the asker's notice names someone or opens the doc: %+v", n)
	}
}

// An answer reaches whoever asked, and only them. Approved, they can read the
// doc: who approved it, its title and what they may do. Declined, or approved
// and taken off again, or when the resolver can't say: nothing about the doc,
// never who answered.
func TestAnAnswerReachesTheAskerAndRevealsNothingTheyCantOpen(t *testing.T) {
	answer := func(asker, status, level string) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: resourceEventBusiness.DocAccessAnswered, ResourceKind: "doc", ResourceUUID: docID,
			Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{asker},
			Data: map[string]string{"request": "r1", "status": status, "level": level}}
	}
	one := func(ts *tellings, asker string) notificationBusiness.Notice {
		t.Helper()
		got := ts.of("doc.access_answered")
		if len(got) != 1 || !slices.Equal(got[0].to, []string{asker + ":subject"}) {
			t.Fatalf("the answer told %+v, want %s alone", got, asker)
		}
		n := got[0].notice
		if n.DedupKey != "doc.access_answered|r1" || n.Email != notificationBusiness.EventDocShared || n.Path != "/app/doc/"+docID {
			t.Errorf("the answer is keyed or sent as %+v", n)
		}
		return n
	}

	// Approved: readID may read it now.
	ts := withNotifySeams(t, nil)
	notify(context.Background(), answer(readID, "approved", "edit"))
	if n := one(ts, readID); n.Title != "Person a1 approved your request for ‘Launch notes’" || n.Body != "You can edit it." ||
		n.ResourceTitle != "Launch notes" || n.Actor == nil || n.Actor.Uuid != ownerID || n.Closed {
		t.Errorf("an approval reads %+v", n)
	}

	// Declined, to someone who can't open it.
	ts = withNotifySeams(t, nil)
	notify(context.Background(), answer(awayID, "declined", "edit"))
	n := one(ts, awayID)
	revealsNothing(t, n)
	if n.Title != "Your request for access wasn't approved" || n.Data["status"] != "declined" || n.Data["level"] != "edit" {
		t.Errorf("a decline reads %+v", n)
	}

	// Approved, then taken off again before the answer was told.
	ts = withNotifySeams(t, nil)
	notify(context.Background(), answer(awayID, "approved", "view"))
	n = one(ts, awayID)
	revealsNothing(t, n)
	if n.Title != "Your request for access was approved" {
		t.Errorf("an approval they can't use reads %+v", n)
	}

	// The resolver can't say: told as if they can't open it.
	ts = withNotifySeams(t, nil)
	decide = func(context.Context, string, []string) (map[string]accessBusiness.Decision, error) {
		return nil, errors.New("graph down")
	}
	notify(context.Background(), answer(readID, "approved", "edit"))
	revealsNothing(t, one(ts, readID))

	// Declined, to someone who reads it already and asked for more: the doc
	// is named, who declined isn't.
	ts = withNotifySeams(t, nil)
	notify(context.Background(), answer(readID, "declined", "edit"))
	if n := one(ts, readID); n.Title != "Your request to edit ‘Launch notes’ wasn't approved" || n.Actor != nil || n.Agent != nil || n.Closed {
		t.Errorf("a decline to a reader reads %+v", n)
	}
}

// The audit says who asked, what was asked for and how it was answered, in
// words (DESIGN-BUILD-8, 23 Oct 2026), and a private page by "a private page"
// alone. An approval that gave another level than the one asked for says
// which it gave.
func TestRequestsAndAnswersAreAudited(t *testing.T) {
	answered := func(status, asked, given string) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: resourceEventBusiness.DocAccessAnswered, ResourceKind: "doc", ResourceUUID: docID,
			Subjects: []string{awayID}, Data: map[string]string{"request": "r1", "status": status, "asked": asked, "level": given}}
	}
	summary, meta := auditRow(answered("declined", "edit", "edit"), privateDoc())
	if summary != "Declined a request to edit a private page" || meta["request"] != "r1" || meta["people"] != awayID {
		t.Errorf("an answer's row: %q %v", summary, meta)
	}
	for _, c := range []struct{ asked, given, want string }{
		{"comment", "comment", "Approved a request to comment on a private page"},
		{"edit", "comment", "Approved a request to edit a private page, with comment access"},
		{"view", "full", "Approved a request to view a private page, with full access"},
	} {
		if summary, _ := auditRow(answered("approved", c.asked, c.given), privateDoc()); summary != c.want {
			t.Errorf("asked %s, given %s: %q, want %q", c.asked, c.given, summary, c.want)
		}
	}
	summary, _ = auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocAccessRequested, ResourceKind: "doc", ResourceUUID: docID,
		Data: map[string]string{"request": "r1", "level": "comment"}}, privateDoc())
	if summary != "Asked to comment on a private page" {
		t.Errorf("a request's row: %q", summary)
	}
}

// A proposal tells the doc's owner after a hand-over, not its maker.
func TestAProposalTellsTheOwnerAfterAHandOver(t *testing.T) {
	ts := withNotifySeams(t, nil)
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		d := privateDoc()
		d.Owner = user(editID)
		return d, nil
	}
	notify(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: readID}, At: noon, Data: map[string]string{"proposal_id": "p9"}})
	if got := ts.of("doc.proposal.created"); len(got) != 1 || !slices.Equal(got[0].to, []string{editID + ":owner"}) {
		t.Errorf("a proposal told %+v, want the new owner", got)
	}
}
