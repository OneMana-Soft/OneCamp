package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ClientReview"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

func TestOneGuestOnOneLinkIsOneAuthor(t *testing.T) {
	link, other := uuid.New(), uuid.New()
	if GuestAuthorID(link, "Dana") != GuestAuthorID(link, " dana ") {
		t.Error("the same guest is two authors")
	}
	if GuestAuthorID(link, "Dana") == GuestAuthorID(link, "Eli") || GuestAuthorID(link, "Dana") == GuestAuthorID(other, "Dana") {
		t.Error("two guests share an author")
	}
	if !strings.HasPrefix(GuestAuthorID(link, "Dana"), "guest-") {
		t.Error("a guest's id isn't in the guest namespace")
	}
}

// replyWorld fakes the doc's decision and the store.
type replyWorld struct {
	level  accessBusiness.Level
	parent *guestModel.GuestComment
	saved  struct {
		label, body string
		author      uuid.UUID
	}
}

func newReplyWorld(t *testing.T) *replyWorld {
	w := &replyWorld{level: accessBusiness.LevelComment}
	prev := [...]any{docDecision, commentOf, saveReply, workspaceName, guestAccessOn}
	docDecision = func(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: w.level}, nil
	}
	commentOf = func(context.Context, string, uuid.UUID) (*guestModel.GuestComment, error) { return w.parent, nil }
	saveReply = func(_ context.Context, p *guestModel.GuestComment, author uuid.UUID, label, body string) (uuid.UUID, time.Time, error) {
		w.saved.label, w.saved.body, w.saved.author = label, body, author
		return uuid.New(), time.Now(), nil
	}
	workspaceName = func() string { return "Acme Studio" }
	guestAccessOn = func() bool { return true }
	t.Cleanup(func() {
		docDecision = prev[0].(func(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error))
		commentOf = prev[1].(func(context.Context, string, uuid.UUID) (*guestModel.GuestComment, error))
		saveReply = prev[2].(func(context.Context, *guestModel.GuestComment, uuid.UUID, string, string) (uuid.UUID, time.Time, error))
		workspaceName = prev[3].(func() string)
		guestAccessOn = prev[4].(func() bool)
	})
	return w
}

func TestAMemberAnswersAGuestWhereTheGuestSeesIt(t *testing.T) {
	w := newReplyWorld(t)
	member := uuid.New()
	who := accessBusiness.Subject{UserUUID: member.String()}
	w.parent = &guestModel.GuestComment{Id: uuid.New(), GrantID: uuid.New(), DocUUID: "d", AuthorKind: guestModel.AuthorGuest}

	got, err := ReplyToGuest(context.Background(), who, "Maya Chen", "d", w.parent.Id, `<b>Thanks</b> — fixed <script>alert(1)</script>`)
	if err != nil {
		t.Fatal(err)
	}
	if w.saved.label != "Maya Chen from Acme Studio" || got.AuthorName != "Maya Chen" || !got.FromTeam || *got.ParentID != w.parent.Id || w.saved.author != member {
		t.Errorf("the reply: %+v, saved %+v", got, w.saved)
	}
	// The guest's own limits: no markup kept.
	if strings.ContainsAny(w.saved.body, "<>") {
		t.Errorf("markup kept: %q", w.saved.body)
	}
	// Never an address, even when the name given is one.
	if _, err := ReplyToGuest(context.Background(), who, "maya@acme.test", "d", w.parent.Id, "ok"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.saved.label, "@") {
		t.Errorf("an address reached the guest: %q", w.saved.label)
	}
	// Too long, as for a guest: cut to the cap, in characters.
	if _, err := ReplyToGuest(context.Background(), who, "Maya", "d", w.parent.Id, strings.Repeat("é", maxGuestCommentLen+50)); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(w.saved.body)); n != maxGuestCommentLen {
		t.Errorf("%d characters kept", n)
	}
}

func TestOnlySomeoneWhoMayCommentAnswersAGuestAndNeverAnAgent(t *testing.T) {
	w := newReplyWorld(t)
	w.parent = &guestModel.GuestComment{Id: uuid.New(), GrantID: uuid.New(), DocUUID: "d", AuthorKind: guestModel.AuthorGuest}
	member := accessBusiness.Subject{UserUUID: uuid.NewString()}
	for _, c := range []struct {
		name  string
		level accessBusiness.Level
		who   accessBusiness.Subject
		want  error
	}{
		{"a reader", accessBusiness.LevelView, member, ErrForbidden},
		{"no access", accessBusiness.LevelNone, member, ErrNotFound},
		{"an agent", accessBusiness.LevelEdit, accessBusiness.Subject{UserUUID: uuid.NewString(), AgentUUID: "captain"}, ErrForbidden},
	} {
		w.level = c.level
		if _, err := ReplyToGuest(context.Background(), c.who, "X", "d", w.parent.Id, "hi"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	w.level = accessBusiness.LevelComment
	// Only a guest's own comment is answered: not a reply, not a stranger's id.
	w.parent = &guestModel.GuestComment{Id: uuid.New(), AuthorKind: guestModel.AuthorMember}
	if _, err := ReplyToGuest(context.Background(), member, "X", "d", w.parent.Id, "hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a reply to a reply: %v", err)
	}
	w.parent = nil
	if _, err := ReplyToGuest(context.Background(), member, "X", "d", uuid.New(), "hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a reply to nothing: %v", err)
	}
	guestAccessOn = func() bool { return false }
	if _, err := ReplyToGuest(context.Background(), member, "X", "d", uuid.New(), "hi"); !errors.Is(err, ErrGuestDisabled) {
		t.Errorf("guest access off: %v", err)
	}
}

func TestAClientGivesADocAVerdictOnlyThroughACommentLink(t *testing.T) {
	prevAdd, prevPost, prevOn := addDocReview, postGuestNote, guestAccessOn
	t.Cleanup(func() { addDocReview, postGuestNote, guestAccessOn = prevAdd, prevPost, prevOn })
	guestAccessOn = func() bool { return true }
	var said string
	addDocReview = func(doc string, _ uuid.UUID, decision, name, note string) (*reviewModel.DocReview, error) {
		return &reviewModel.DocReview{DocUUID: doc, Decision: decision, Name: name, Note: note, CreatedAt: time.Now()}, nil
	}
	postGuestNote = func(_ context.Context, _ *guestModel.GuestGrant, _ string, text string) (*GuestCommentView, error) {
		said = text
		return &GuestCommentView{}, nil
	}
	view := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, ResourceID: "d", Capability: guestModel.CapabilityView}
	if _, err := ReviewDocAsGuest(context.Background(), view, "Dana", reviewModel.Approved, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a view link gave a verdict: %v", err)
	}
	comment := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, ResourceID: "d", Capability: guestModel.CapabilityComment}
	got, err := ReviewDocAsGuest(context.Background(), comment, "Dana", reviewModel.Changes, "The second section is wrong")
	if err != nil || got.Decision != reviewModel.Changes || said != "Changes requested: The second section is wrong" {
		t.Errorf("a verdict: %+v %v, said %q", got, err, said)
	}
	if _, err := ReviewDocAsGuest(context.Background(), comment, "Dana", reviewModel.Changes, ""); err == nil {
		t.Error("changes asked for without saying which")
	}
}
