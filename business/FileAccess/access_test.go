package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
)

const (
	me        = "11111111-1111-4111-8111-111111111111"
	ann       = "22222222-2222-4222-8222-222222222222"
	ben       = "33333333-3333-4333-8333-333333333333"
	mine      = "44444444-4444-4444-8444-444444444444" // a channel, project, doc or board I'm in
	elsewhere = "55555555-5555-4555-8555-555555555555" // one I'm not
	broken    = "66666666-6666-4666-8666-666666666666" // one the graph couldn't answer for
)

var errGraph = errors.New("the graph didn't answer")

// fake answers each place's own rule: I am in "mine" (and in the group
// started), not in "elsewhere", and "broken" can't be asked.
func fake() sources {
	yesNo := func(_ context.Context, userUUID, id string) (bool, error) {
		if id == broken {
			return false, errGraph
		}
		return userUUID == me && id == mine, nil
	}
	started := mustGroup(me, ann, ben)
	doc := func(_ context.Context, userUUID, id string) error {
		switch {
		case id == broken:
			return errGraph
		case id == elsewhere:
			return docBusiness.ErrNotFound
		case userUUID != me:
			return ErrRefused
		}
		return nil
	}
	return sources{
		readsChannel: yesNo, writesChannel: yesNo, inProject: yesNo, readsBoard: yesNo, editsBoard: yesNo,
		inGroup: func(_ context.Context, userUUID, groupID string) (bool, bool, error) {
			if groupID != started {
				return false, false, nil
			}
			return true, userUUID == me, nil
		},
		readsDoc: doc, addsToDoc: doc,
	}
}

func mustGroup(people ...string) string {
	id, err := helpers.GenerateGroupID(people)
	if err != nil {
		panic(err)
	}
	return id
}

// Every kind of place answers by its own rule, for having its files and for
// adding one: yes for someone in it, no for anyone else, and the lookup's
// own error when it couldn't be asked.
func TestAFileFollowsItsPlacesRule(t *testing.T) {
	s := fake()
	ctx := context.Background()
	dm := helpers.GetGroupingId(me, ann)
	group := mustGroup(me, ann, ben)
	cases := []struct {
		name     string
		who      string
		place    Place
		want     error
		wantRead error
	}{
		{"a channel I'm in", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHANNEL, ID: mine}, nil, nil},
		{"a channel I'm not in", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHANNEL, ID: elsewhere}, ErrRefused, ErrRefused},
		{"a channel the graph can't answer for", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHANNEL, ID: broken}, errGraph, errGraph},
		{"a channel named any other way", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHANNEL, ID: "{" + mine + "}"}, ErrRefused, ErrRefused},
		{"a project I'm in", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_PROJECT, ID: mine}, nil, nil},
		{"a project I'm not in", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_PROJECT, ID: elsewhere}, ErrRefused, ErrRefused},
		{"a board I may change", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_BOARD, ID: mine}, nil, nil},
		{"a board I may not", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_BOARD, ID: elsewhere}, ErrRefused, ErrRefused},
		{"my conversation with Ann", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: dm}, nil, nil},
		{"Ann's conversation with Ben", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: helpers.GetGroupingId(ann, ben)}, ErrRefused, ErrRefused},
		{"a conversation with myself", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: helpers.GetGroupingId(me, me)}, nil, nil},
		{"a conversation id written out of order", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: ann + " " + me}, ErrRefused, ErrRefused},
		{"a conversation id of three people", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: dm + " " + ben}, ErrRefused, ErrRefused},
		{"a group I'm in", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: group}, nil, nil},
		{"a group I'm not in", ann, Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: group}, ErrRefused, ErrRefused},
		{"a group id that isn't one", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: "not-a-group"}, ErrRefused, ErrRefused},
		{"a doc I may comment on", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: mine}, nil, nil},
		{"a doc I may only see", ann, Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: mine}, ErrRefused, ErrRefused},
		{"a doc that isn't there for me", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: elsewhere}, docBusiness.ErrNotFound, docBusiness.ErrNotFound},
		{"a doc id that isn't one", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: "nope"}, docBusiness.ErrNotFound, docBusiness.ErrNotFound},
		{"nobody's (public)", ann, Place{Kind: postgresStruct.ATTACHMENT_SRC_PUBLIC}, nil, nil},
		{"a kind with no rule (an import's task)", me, Place{Kind: postgresStruct.ATTACHMENT_SRC_TASK, ID: mine}, ErrRefused, ErrRefused},
		{"no kind", me, Place{ID: mine}, ErrRefused, ErrRefused},
	}
	for _, c := range cases {
		if got := s.mayAdd(ctx, c.who, c.place); !errors.Is(got, c.want) || (c.want == nil && got != nil) {
			t.Errorf("%s: adding a file = %v, want %v", c.name, got, c.want)
		}
		if got := s.mayRead(ctx, c.who, c.place); !errors.Is(got, c.wantRead) || (c.wantRead == nil && got != nil) {
			t.Errorf("%s: having its files = %v, want %v", c.name, got, c.wantRead)
		}
	}
}

// A group is written at its first message, and its id is made from its people
// before then, so a file can go to a group not started yet. The upload names
// the group's people instead: they must make its id and include the uploader,
// as the group's first message must. A group that has started is asked who is
// in it, whatever people an upload names. Nobody has the files of a group not
// started yet.
func TestAFileForAGroupNotStartedYetNamesItsPeople(t *testing.T) {
	s := fake()
	ctx := context.Background()
	fresh := mustGroup(me, ann, "77777777-7777-4777-8777-777777777777")
	cases := []struct {
		name   string
		place  Place
		wanted error
	}{
		{"its people, me among them", Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: fresh, People: []string{ann, me, "77777777-7777-4777-8777-777777777777"}}, nil},
		{"its people as the web writes them", Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: fresh, People: []string{" " + ann, "77777777-7777-4777-8777-777777777777", me + " "}}, nil},
		{"no people", Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: fresh}, ErrRefused},
		{"people who make another id", Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: fresh, People: []string{me, ann}}, ErrRefused},
		{"a group of others, named by their people", Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: mustGroup(ann, ben, "77777777-7777-4777-8777-777777777777"), People: []string{ann, ben, "77777777-7777-4777-8777-777777777777"}}, ErrRefused},
	}
	for _, c := range cases {
		if got := s.mayAdd(ctx, me, c.place); !errors.Is(got, c.wanted) || (c.wanted == nil && got != nil) {
			t.Errorf("%s: %v, want %v", c.name, got, c.wanted)
		}
	}
	// A started group is asked, whatever an upload says of its people.
	started := Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: mustGroup(me, ann, ben), People: []string{me, ann, ben}}
	if got := s.mayAdd(ctx, ann, started); !errors.Is(got, ErrRefused) {
		t.Errorf("Ann adding to the started group she isn't in, naming its people: %v", got)
	}
	if got := s.mayRead(ctx, me, Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: fresh, People: []string{me, ann, "77777777-7777-4777-8777-777777777777"}}); !errors.Is(got, ErrRefused) {
		t.Errorf("having the files of a group not started yet: %v", got)
	}
}

func TestAConversationsFilesAreItsKinds(t *testing.T) {
	dm := helpers.GetGroupingId(me, ann)
	if p := ConversationOf(dm); p.Kind != postgresStruct.ATTACHMENT_SRC_CHAT || p.ID != dm {
		t.Errorf("a direct conversation: %+v", p)
	}
	group := mustGroup(me, ann, ben)
	if p := ConversationOf(group); p.Kind != postgresStruct.ATTACHMENT_SRC_GRP_CHAT || p.ID != group {
		t.Errorf("a group: %+v", p)
	}
}

func TestOnlyAnswersAreRefusals(t *testing.T) {
	for _, err := range []error{ErrRefused, ErrNoSuchFile, docBusiness.ErrNotFound, docBusiness.ErrDocDeleted} {
		if !IsRefusal(err) {
			t.Errorf("%v is an answer", err)
		}
	}
	if IsRefusal(errGraph) || IsRefusal(nil) {
		t.Error("a lookup that failed, or a yes, is no refusal")
	}
}

// A task's files and its comments' are stored as its project's, and go to
// whoever may read the task: a project's file on a task kept from someone is,
// to them, a file that isn't there. A file of any other place is no task's,
// and nothing is asked about it.
func TestAProjectsFileOnATaskKeptFromSomeoneIsntThereForThem(t *testing.T) {
	const keptFile, openFile = "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888"
	asked := 0
	s := fake()
	s.keepsTaskFile = func(_ context.Context, userUUID string, file *attachmentModels.Attachment) (bool, error) {
		asked++
		fileID := file.Uuid.String()
		if fileID == broken {
			return false, errGraph
		}
		return userUUID == ben && fileID == keptFile, nil
	}
	ctx := context.Background()
	project := Place{Kind: postgresStruct.ATTACHMENT_SRC_PROJECT, ID: mine}
	for _, c := range []struct {
		name string
		who  string
		file string
		want error
	}{
		{"a file of a task kept from ben, to ben", ben, keptFile, ErrNoSuchFile},
		{"the same file, to someone who may read the task", me, keptFile, nil},
		{"a file of a task open to the project, to ben", ben, openFile, nil},
		{"a file the graph couldn't answer for", ben, broken, errGraph},
	} {
		if got := s.taskFile(ctx, c.who, project, fileOf(c.file)); !errors.Is(got, c.want) || (c.want == nil && got != nil) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	asked = 0
	for _, kind := range []string{postgresStruct.ATTACHMENT_SRC_CHANNEL, postgresStruct.ATTACHMENT_SRC_CHAT, postgresStruct.ATTACHMENT_SRC_GRP_CHAT,
		postgresStruct.ATTACHMENT_SRC_DOC, postgresStruct.ATTACHMENT_SRC_BOARD, postgresStruct.ATTACHMENT_SRC_PUBLIC} {
		if got := s.taskFile(ctx, ben, Place{Kind: kind, ID: mine}, fileOf(keptFile)); got != nil {
			t.Errorf("a %s's file: %v, want nil", kind, got)
		}
	}
	if asked != 0 {
		t.Errorf("files of places that aren't projects asked about tasks %d times", asked)
	}
}

// fileOf is a project's file row with id, as FileOf answers it.
func fileOf(id string) *attachmentModels.Attachment {
	return &attachmentModels.Attachment{Uuid: uuid.MustParse(id), SrcKey: postgresStruct.ATTACHMENT_SRC_PROJECT, SrcValue: mine}
}
