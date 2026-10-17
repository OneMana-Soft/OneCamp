package business

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A doc read by ten people and commented on by one: a new comment reaches its
// owner and that commenter, never the ten (D-13).
func TestWithTenReadersAndOneCommenterOnlyTheOwnerAndTheCommenterHear(t *testing.T) {
	yes := true
	doc := &dgraphStruct.DgraphDoc{Uuid: "doc", IsPrivate: &yes, CreatedBy: person("owner"),
		Comments: []*dgraphStruct.DgraphComment{commentBy("commenter")}}
	for i := 0; i < 10; i++ {
		doc.ReadingUser = append(doc.ReadingUser, person(fmt.Sprintf("reader%d", i)))
	}
	doc.CommentingUser = []*dgraphStruct.DgraphUser{person("commenter")}
	// reader3 comments now.
	got := docCommentAudience(doc, nil, "reader3", nil, listRule(doc))
	if want := []string{"owner", "commenter"}; !slices.Equal(got, want) {
		t.Errorf("told %v, want %v", got, want)
	}
}

// Follows on top of the participants: "none" silences a participant unless
// they're mentioned, "all" adds anyone who may read the doc, "mentions" is the
// participants rule itself.
func TestFollowModesShapeWhoHearsOfAComment(t *testing.T) {
	follows := map[string]string{
		"editor":    "none",     // a participant who muted the doc
		"commenter": "none",     // muted, but mentioned below
		"viewer":    "all",      // a reader who follows everything
		"stranger":  "all",      // follows, but can't read the private doc
		"editor2":   "mentions", // the default, chosen
		"reader":    "mentions", // replies and mentions: not a participant, not mentioned
	}
	got := docCommentAudience(discussedDoc(), []string{"commenter"}, "owner", follows, listRule(discussedDoc()))
	if want := []string{"editor2", "commenter", "viewer"}; !slices.Equal(got, want) {
		t.Errorf("told %v, want %v", got, want)
	}
	// With no follows read, the participants rule is unchanged.
	if got, want := docCommentAudience(discussedDoc(), nil, "owner", nil, listRule(discussedDoc())), docCommentRecipients(discussedDoc(), nil, "owner", listRule(discussedDoc())); !slices.Equal(got, want) {
		t.Errorf("with no follows told %v, want the participants %v", got, want)
	}
}

// The inbox gets an item for everyone told but the doc's owner and the people
// mentioned, whose feed lists the comment already; a follower is told why.
func TestACommentsInboxNoticesAreForThoseTheFeedHasNoItemFor(t *testing.T) {
	var mu sync.Mutex
	var to []notificationBusiness.NoticeRecipient
	var told notificationBusiness.Notice
	old := docCommentInbox
	docCommentInbox = func(_ context.Context, n notificationBusiness.Notice, r []notificationBusiness.NoticeRecipient) {
		mu.Lock()
		defer mu.Unlock()
		told, to = n, r
	}
	defer func() { docCommentInbox = old }()

	doc := discussedDoc()
	recipients := []string{"owner", "editor2", "commenter", "reader", "viewer"}
	tellDocCommentInbox(context.Background(), doc, person("editor"), "c1", []string{"reader"}, recipients, map[string]string{"viewer": "all"}, listRule(doc))
	var got []string
	for _, r := range to {
		got = append(got, r.UserUUID+":"+r.Reason)
		if !r.InboxOnly {
			t.Errorf("%s's inbox notice is also pushed or emailed; the comment's own push and email do that", r.UserUUID)
		}
	}
	if want := []string{"editor2:participant", "commenter:participant", "viewer:follower"}; !slices.Equal(got, want) {
		t.Errorf("inbox notices for %v, want %v", got, want)
	}
	if told.Kind != "doc.comment.created" || told.DedupKey != "comment|c1" || told.Data["comment_uuid"] != "c1" || told.ResourceTitle != "Launch plan" {
		t.Errorf("the notice is %+v", told)
	}
}

// A comment reaches people through the bus: the sink reads back its author
// and words and tells the participants, and only through the bus.
func TestACommentIsToldThroughTheBus(t *testing.T) {
	s := &sent{}
	withSends(t, s)
	oldA, oldT := docCommentAuthor, docCommentText
	defer func() { docCommentAuthor, docCommentText = oldA, oldT }()
	docCommentAuthor = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uuid: id, UserName: id}, nil
	}
	docCommentText = func(context.Context, string) (string, error) { return "<p>looks good</p>", nil }

	// The emit carries ids, never the words.
	var emitted []resourceEventBusiness.Event
	oldBus := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = busFunc(func(_ context.Context, e resourceEventBusiness.Event) { emitted = append(emitted, e) })
	emitDocComment(context.Background(), "doc", "editor", "c1", time.Now(), []*dgraphStruct.DgraphUser{person("reader"), nil})
	resourceEventBusiness.Bus = oldBus
	if len(emitted) != 1 {
		t.Fatalf("emitted %d events", len(emitted))
	}
	e := emitted[0]
	if e.Kind != resourceEventBusiness.DocCommentCreated || e.Actor.UserUUID != "editor" || !slices.Equal(e.Subjects, []string{"reader"}) ||
		e.Data["comment_uuid"] != "c1" || len(e.Data) != 1 {
		t.Fatalf("the event is %+v", e)
	}

	notifyDocComment(context.Background(), e)
	if want := []string{"owner", "editor2", "commenter", "reader"}; !slices.Equal(s.emailed, want) {
		t.Errorf("emailed %v, want %v", s.emailed, want)
	}
	if !slices.Contains(s.pushedTo, "device-of-reader") || slices.Contains(s.pushedTo, "device-of-editor") {
		t.Errorf("pushed to %v", s.pushedTo)
	}
}

type busFunc func(context.Context, resourceEventBusiness.Event)

func (f busFunc) Emit(ctx context.Context, e resourceEventBusiness.Event) { f(ctx, e) }

// A reply in a thread is told by the thread's event, not doc-wide: its comment
// event says so, and the comment sink leaves it.
func TestAThreadReplyIsNotToldDocWide(t *testing.T) {
	var emitted []resourceEventBusiness.Event
	oldBus := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = busFunc(func(_ context.Context, e resourceEventBusiness.Event) { emitted = append(emitted, e) })
	defer func() { resourceEventBusiness.Bus = oldBus }()
	ctx := context.WithValue(context.Background(), ThreadReplyKey, "t1")
	emitDocComment(ctx, "doc", "editor", "c2", time.Now(), nil)
	emitDocComment(context.Background(), "doc", "editor", "c3", time.Now(), nil)
	if emitted[0].Data["thread_reply"] != "true" || emitted[0].Data["thread_id"] != "t1" || emitted[1].Data["thread_reply"] != "" {
		t.Fatalf("events %+v", emitted)
	}
	if wantsDocComment(emitted[0]) || !wantsDocComment(emitted[1]) {
		t.Error("the comment sink took a thread reply, or left a page comment")
	}
}
