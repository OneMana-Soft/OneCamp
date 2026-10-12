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

	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
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
		got := docCommentRecipients(c.doc, c.mentioned, c.author)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: told %v, want %v", c.name, got, c.want)
		}
	}
	if docCommentRecipients(nil, []string{"reader"}, "editor") != nil {
		t.Error("a doc that couldn't be read told someone")
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
	participantsError error
}

func withSends(t *testing.T, s *sent) {
	t.Helper()
	oldP, oldD, oldPush, oldE, oldI := docCommentParticipants, docCommentDevices, docCommentPush, docCommentEmail, docCommentIcon
	t.Cleanup(func() {
		docCommentParticipants, docCommentDevices, docCommentPush, docCommentEmail, docCommentIcon = oldP, oldD, oldPush, oldE, oldI
	})
	docCommentParticipants = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		if s.participantsError != nil {
			return nil, s.participantsError
		}
		return discussedDoc(), nil
	}
	docCommentDevices = func(_ context.Context, ids []string) ([]string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.askedDevicesFor = append(s.askedDevicesFor, ids...)
		if s.devicesErr != nil {
			return nil, s.devicesErr
		}
		var tokens []string
		for _, id := range ids {
			tokens = append(tokens, "device-of-"+id)
		}
		return tokens, nil
	}
	docCommentPush = func(_ context.Context, _ map[string]string, tokens []string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pushedTo = append(s.pushedTo, tokens...)
		return s.push
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
		{"the push fails", nil, errors.New("firebase down"), true, true},
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

	// Who takes part can't be read: nobody is told, rather than anyone wrongly.
	s := &sent{participantsError: errors.New("graph down")}
	withSends(t, s)
	sendNewDocCommentNotification("Comment - Ed", "looks good", "c1", &dgraphStruct.DgraphDoc{Uuid: "doc"}, person("editor"), nil)
	if len(s.askedDevicesFor) > 0 || len(s.emailed) > 0 {
		t.Errorf("with the graph down, pushed to %v and emailed %v", s.askedDevicesFor, s.emailed)
	}
}

// Nothing about a doc comment goes through a pause: devices come from the
// lookup that holds back anyone who paused notifications, is in quiet hours
// or is in focus time, whoever wrote it. Only a person's own "notify anyway"
// goes through, and an agent's never does (business/Notification urgent.go).
func TestNothingAboutADocCommentGoesThroughAPause(t *testing.T) {
	if reflect.ValueOf(docCommentDevices).Pointer() != reflect.ValueOf(userFCMtokenBusiness.GetFCMTokenByListOfUserId).Pointer() {
		t.Error("doc comment pushes look up devices some other way than the paused-aware lookup")
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
