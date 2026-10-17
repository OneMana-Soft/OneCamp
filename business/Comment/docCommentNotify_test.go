package business

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func person(id string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{Uuid: id} }

func commentBy(id string) *dgraphStruct.DgraphComment {
	return &dgraphStruct.DgraphComment{CommentBy: person(id)}
}

// a private doc: owner and two editors; a viewer and a mentionable reader; a
// commenter; and someone who commented before it stopped being shared with
// them.
func discussedDoc() *dgraphStruct.DgraphDoc {
	yes := true
	return &dgraphStruct.DgraphDoc{Uuid: "doc", Title: "Launch plan", IsPrivate: &yes, CreatedBy: person("owner"),
		EditingUser:    []*dgraphStruct.DgraphUser{person("owner"), person("editor"), person("editor2")},
		ReadingUser:    []*dgraphStruct.DgraphUser{person("viewer"), person("reader")},
		CommentingUser: []*dgraphStruct.DgraphUser{person("commenter")},
		Comments:       []*dgraphStruct.DgraphComment{commentBy("commenter"), commentBy("former"), commentBy("editor")}}
}

// listRule is who may read a test's doc by its lists alone: on a private doc
// its maker and the people on its lists, on any other anyone, on a deleted doc
// nobody. The server asks the Access Resolver's rule instead (business/Doc
// Readers, docReaders here), which counts the doc's teams, its space and the
// grants that have ended too.
func listRule(doc *dgraphStruct.DgraphDoc) func(string) bool {
	return func(id string) bool {
		if doc == nil || id == "" || helpers.IsSoftDeleted(doc.DeletedAt) {
			return false
		}
		if doc.IsPrivate == nil || !*doc.IsPrivate {
			return true
		}
		if doc.CreatedBy != nil && doc.CreatedBy.Uuid == id {
			return true
		}
		for _, list := range [][]*dgraphStruct.DgraphUser{doc.EditingUser, doc.ReadingUser, doc.CommentingUser} {
			for _, u := range list {
				if u != nil && u.Uuid == id {
					return true
				}
			}
		}
		return false
	}
}

// Who hears about a new comment on a doc: its owner, its editors, whoever
// commented before, and anyone mentioned, never the author, and only people
// who may read the doc now. When the owner commented, nobody was told; an
// editor's comment was pushed to their own devices.
func TestADocCommentIsForThePeopleTakingPartInTheDoc(t *testing.T) {
	no := false
	deleted := time.Now().Add(-time.Minute)
	public := discussedDoc()
	public.IsPrivate = &no
	gone := discussedDoc()
	gone.DeletedAt = &deleted
	for _, c := range []struct {
		name, author string
		doc          *dgraphStruct.DgraphDoc
		mentioned    []string
		want         []string
	}{
		{"its owner comments", "owner", discussedDoc(), nil, []string{"editor", "editor2", "commenter"}},
		{"an editor comments", "editor", discussedDoc(), nil, []string{"owner", "editor2", "commenter"}},
		{"an editor mentions a reader and a stranger", "editor", discussedDoc(), []string{"reader", "stranger", "owner"},
			[]string{"owner", "editor2", "commenter", "reader"}},
		{"a reader comments", "viewer", discussedDoc(), nil, []string{"owner", "editor", "editor2", "commenter"}},
		{"on a public doc, a mention reaches anyone", "commenter", public, []string{"stranger"},
			[]string{"owner", "editor", "editor2", "former", "stranger"}},
		{"on a deleted doc, nobody", "editor", gone, []string{"reader"}, nil},
	} {
		got := docCommentRecipients(c.doc, c.mentioned, c.author, listRule(c.doc))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: told %v, want %v", c.name, got, c.want)
		}
	}
	if docCommentRecipients(nil, []string{"reader"}, "editor", listRule(nil)) != nil {
		t.Error("a doc that couldn't be read told someone")
	}
	if docCommentRecipients(discussedDoc(), []string{"reader"}, "editor", nil) != nil {
		t.Error("with no read rule, someone was told")
	}
}

type sent struct {
	mu                sync.Mutex
	askedDevicesFor   []string
	pushedTo          []string
	emailed           []string
	emailedActor      string
	emailedTitle      string
	devicesErr, push  error
	noticeID          string
	participantsError error
	// readers is the read rule's answer for each person; nil asks the doc's
	// lists (listRule). readersAsked is who it was asked about.
	readers      map[string]bool
	readersErr   error
	readersAsked []string
}

func withSends(t *testing.T, s *sent) {
	t.Helper()
	oldP, oldD, oldE, oldI, oldR := docCommentParticipants, docCommentDeliver, docCommentEmail, docCommentIcon, docReaders
	t.Cleanup(func() {
		docCommentParticipants, docCommentDeliver, docCommentEmail, docCommentIcon, docReaders = oldP, oldD, oldE, oldI, oldR
	})
	docReaders = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.readersAsked = append(s.readersAsked, ids...)
		if s.readersErr != nil {
			return nil, s.readersErr
		}
		out := map[string]bool{}
		for _, id := range ids {
			if s.readers != nil {
				out[id] = s.readers[id]
			} else {
				out[id] = listRule(discussedDoc())(id)
			}
		}
		return out, nil
	}
	docCommentParticipants = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		if s.participantsError != nil {
			return nil, s.participantsError
		}
		return discussedDoc(), nil
	}
	// Deliver, as each person's device: its read or its push may fail, which
	// Deliver logs, and the email goes all the same.
	docCommentDeliver = func(_ context.Context, ids []string, _ map[string]string, noticeID string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.askedDevicesFor = append(s.askedDevicesFor, ids...)
		s.noticeID = noticeID
		if s.devicesErr != nil {
			return
		}
		for _, id := range ids {
			s.pushedTo = append(s.pushedTo, "device-of-"+id)
		}
	}
	docCommentEmail = func(actorUUID, _, _, _, docTitle, _, _ string, recipients []string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.emailedActor, s.emailedTitle = actorUUID, docTitle
		s.emailed = append(s.emailed, recipients...)
	}
	docCommentIcon = func(context.Context, *string) string { return "" }
}

// An editor's comment is pushed to the others' devices, never the editor's
// own, and emailed to the same people. Push and email are independent: a
// device lookup or a push that fails still sends the email, which it didn't.
func TestADocCommentIsPushedAndEmailedEachWayOnItsOwn(t *testing.T) {
	want := []string{"owner", "editor2", "commenter"}
	for _, c := range []struct {
		name               string
		devicesErr, push   error
		pushed, emailedAll bool
	}{
		{"both work", nil, nil, true, true},
		{"the device lookup fails", errors.New("postgres down"), nil, false, true},
		{"the push fails", nil, errors.New("firebase down"), false, true},
	} {
		s := &sent{devicesErr: c.devicesErr, push: c.push}
		withSends(t, s)
		sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), nil)
		if !slices.Equal(s.askedDevicesFor, want) {
			t.Errorf("%s: asked for the devices of %v, want %v", c.name, s.askedDevicesFor, want)
		}
		if c.pushed {
			wantTokens := []string{"device-of-owner", "device-of-editor2", "device-of-commenter"}
			if !slices.Equal(s.pushedTo, wantTokens) {
				t.Errorf("%s: pushed to %v, want %v", c.name, s.pushedTo, wantTokens)
			}
		}
		if slices.Contains(s.pushedTo, "device-of-editor") {
			t.Errorf("%s: the comment was pushed to its author's own devices", c.name)
		}
		if !slices.Equal(s.emailed, want) || s.emailedActor != "editor" || s.emailedTitle != "Launch plan" {
			t.Errorf("%s: emailed %v as %q about %q, want %v as the editor about the doc", c.name, s.emailed, s.emailedActor, s.emailedTitle, want)
		}
	}

	// The desktop app raises a comment once: its notice is named by the doc
	// and the comment.
	s0 := &sent{}
	withSends(t, s0)
	sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), nil)
	if want := firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT + ":doc:c1"; s0.noticeID != want {
		t.Errorf("the comment's notice is named %q, want %q: its type, the doc and the comment", s0.noticeID, want)
	}

	// Who takes part can't be read: nobody is told, rather than anyone wrongly.
	s := &sent{participantsError: errors.New("graph down")}
	withSends(t, s)
	sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), nil)
	if len(s.askedDevicesFor) > 0 || len(s.emailed) > 0 {
		t.Errorf("with the graph down, pushed to %v and emailed %v", s.askedDevicesFor, s.emailed)
	}
}

// Who may read the doc is the Access Resolver's answer, not the doc's lists
// (A5×A6): someone it reaches through a team or its space is told of a
// mention, an editor whose grant has ended isn't, and the rule is asked once,
// about everyone the comment could reach. With no answer, or no rule handed
// over yet, nobody is told.
func TestADocCommentReachesWhomTheAccessRuleLetsRead(t *testing.T) {
	s := &sent{readers: map[string]bool{"owner": true, "commenter": true, "teammate": true, "spacemate": true}}
	withSends(t, s)
	mentions := []*dgraphStruct.DgraphUser{person("teammate"), person("spacemate"), person("leftspace")}
	sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), mentions)
	// editor2's grant ended (the rule says no); "former" can't read it either.
	if want := []string{"owner", "commenter", "teammate", "spacemate"}; !slices.Equal(s.emailed, want) {
		t.Errorf("emailed %v, want %v", s.emailed, want)
	}
	for _, id := range []string{"owner", "editor", "editor2", "commenter", "former", "teammate", "spacemate", "leftspace"} {
		if !slices.Contains(s.readersAsked, id) {
			t.Errorf("the read rule wasn't asked about %s (asked %v)", id, s.readersAsked)
		}
	}

	for name, s := range map[string]*sent{"the rule fails": {readersErr: errors.New("graph down")}} {
		withSends(t, s)
		sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), mentions)
		if len(s.askedDevicesFor) > 0 || len(s.emailed) > 0 {
			t.Errorf("%s: pushed to %v and emailed %v", name, s.askedDevicesFor, s.emailed)
		}
	}
	s = &sent{}
	withSends(t, s)
	docReaders = nil
	sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), mentions)
	if len(s.askedDevicesFor) > 0 || len(s.emailed) > 0 {
		t.Errorf("with no read rule handed over, pushed to %v and emailed %v", s.askedDevicesFor, s.emailed)
	}
}

// Nothing about a doc comment goes through a pause: devices come from the
// lookup that holds back anyone who paused notifications, is in quiet hours
// or is in focus time, whoever wrote it. Only a person's own "notify anyway"
// goes through, and an agent's never does (business/Notification urgent.go).
func TestNothingAboutADocCommentGoesThroughAPause(t *testing.T) {
	if reflect.ValueOf(docCommentDeliver).Pointer() != reflect.ValueOf(notificationBusiness.Deliver).Pointer() {
		t.Error("doc comment pushes go some other way than Deliver, the push gate's")
	}
	for _, dir := range []string{".", "../Doc"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(src), "IgnoringPause") {
				t.Errorf("%s reaches devices through a pause", f)
			}
		}
	}
}

// After a hand-over the doc's owner is its new one (doc_owner): told of a
// comment as the owner, with an inbox item (the feed lists comments on what
// someone made, so the maker has one, and the new owner doesn't). A maker who
// lost the doc isn't told; full-access people take part as editors do.
func TestAfterAHandOverTheNewOwnerTakesPart(t *testing.T) {
	doc := discussedDoc()
	doc.Owner = person("heir")
	doc.FullAccessUsers = []*dgraphStruct.DgraphUser{person("deputy")}
	readable := func(id string) bool { return id != "owner" } // the maker lost it
	got := docCommentRecipients(doc, nil, "editor", readable)
	if len(got) < 2 || got[0] != "heir" || got[1] != "deputy" || slices.Contains(got, "owner") {
		t.Errorf("told %v: the heir first, then full access; never the maker who lost it", got)
	}
	var to []notificationBusiness.NoticeRecipient
	old := docCommentInbox
	docCommentInbox = func(_ context.Context, _ notificationBusiness.Notice, r []notificationBusiness.NoticeRecipient) {
		to = r
	}
	defer func() { docCommentInbox = old }()
	tellDocCommentInbox(context.Background(), doc, person("editor"), "c1", nil, got, nil, readable)
	var ids []string
	for _, r := range to {
		ids = append(ids, r.UserUUID)
	}
	if !slices.Contains(ids, "heir") {
		t.Errorf("inbox items for %v: the new owner has no feed item, so gets one", ids)
	}
}
