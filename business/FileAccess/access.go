// Package business (FileAccess) decides who may add a file to a place, and
// who may have one.
//
// A file (an attachments row) is the file of the place its src_key and
// src_value name: a channel, a conversation with one person ("chat", named by
// its grouping id) or with several ("grpChat"), a project, a doc, a board, or
// nobody's ("public": a profile picture, an app's icon). It is served to
// whoever may see that place, so adding one is writing in it.
//
// Each kind of place already has its rule for who sees it and who writes in
// it, and its files follow that rule: nothing here decides anything of its
// own. It maps each kind onto the rule, and refuses a kind it doesn't know.
// POST /uploadFile asks MayAdd about every place a file goes to; the routes
// that hand a file out, and the assistant's reading of one, ask ReadFile; an
// agent reads a message's pictures through its conversation (FileOf).
//
// Before, an upload was checked only for a doc: anyone signed in could put a
// file into a channel, a conversation, a project or a board they weren't in,
// by naming it.
package business

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	projectModels "github.com/akashc777/OneCamp/models/dgraph/Project"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	"github.com/google/uuid"
)

// A Place is what a file belongs to: its kind (src_key) and its id
// (src_value). People are the account ids of a group conversation's people,
// for an upload to a group that hasn't started yet (MayAdd).
type Place struct {
	Kind   string
	ID     string
	People []string
}

// ConversationOf is the place a conversation's files belong to, by its
// grouping id: a direct conversation's ("chat") when the id names two people
// (helpers.DMPeople), a group's ("grpChat") otherwise.
func ConversationOf(groupingID string) Place {
	if _, _, ok := helpers.DMPeople(groupingID); ok {
		return Place{Kind: postgresStruct.ATTACHMENT_SRC_CHAT, ID: groupingID}
	}
	return Place{Kind: postgresStruct.ATTACHMENT_SRC_GRP_CHAT, ID: groupingID}
}

// ErrRefused is a no: the person may not have the place's files, or may not
// add one to it. For every kind but a doc, a place that isn't there answers
// the same, so a refusal says nothing about the place.
var ErrRefused = errors.New("not allowed")

// ErrNoSuchFile is a file that isn't there, or isn't the place's: one answer
// for both, so a file's id says nothing about other places.
var ErrNoSuchFile = errors.New("no such file")

// IsRefusal reports whether err is an answer (ErrRefused, ErrNoSuchFile, or a
// doc's own) rather than a lookup that failed.
func IsRefusal(err error) bool {
	return errors.Is(err, ErrRefused) || errors.Is(err, ErrNoSuchFile) ||
		errors.Is(err, docBusiness.ErrNotFound) || errors.Is(err, docBusiness.ErrDocDeleted)
}

// MayRead answers whether the person with userUUID (their account id) may
// have the files of place: nil when they may, ErrRefused when they may not,
// and for a doc the doc's own answers, docBusiness.ErrNotFound (it isn't
// there for them) and docBusiness.ErrDocDeleted (it was, and is deleted).
// Any other error is a lookup that failed, and says nothing either way.
func MayRead(ctx context.Context, userUUID string, place Place) error {
	return live.mayRead(ctx, userUUID, place)
}

// MayAdd is MayRead for adding a file to place, which is writing in it: a
// channel's members, while it isn't archived; the people in a conversation; a
// project's members and admins; whoever may comment on a doc; whoever may
// change a board's canvas. Anyone signed in may add a public file.
//
// A group is written at its first message, and the web makes its id from its
// people before that (helpers.GenerateGroupID), so a file can be added to one
// that hasn't started yet. Such a group can't be asked who is in it: the
// upload names its people instead, which must give its id and include the
// uploader, as its first message must.
func MayAdd(ctx context.Context, userUUID string, place Place) error {
	return live.mayAdd(ctx, userUUID, place)
}

// ReadFile is the file with id (its attachments row) for the person, as a
// file of place: MayRead's answer when they may not have the place's files,
// and ErrNoSuchFile when the file isn't the place's (FileOf).
func ReadFile(ctx context.Context, userUUID string, place Place, id string) (*attachmentModels.Attachment, error) {
	if err := MayRead(ctx, userUUID, place); err != nil {
		return nil, err
	}
	return FileOf(ctx, place, id)
}

// FileOf is the file with id when it is place's: its attachments row, written
// at upload, names the place. It decides nothing about who is asking. A file
// is reached through its place, never by an id or a storage key alone: the
// files a message lists are written as its sender sent them, and could name
// any file.
func FileOf(ctx context.Context, place Place, id string) (*attachmentModels.Attachment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNoSuchFile
	}
	att, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, id, place.Kind)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && (att == nil || att.SrcValue != place.ID || att.ObjKey == ""):
		return nil, ErrNoSuchFile
	case err != nil:
		return nil, err
	}
	return att, nil
}

// sources are where the answers come from: each place's own rule, everywhere
// but tests. The yes-or-no ones answer false for a place that isn't there.
type sources struct {
	readsChannel  func(ctx context.Context, userUUID, channelID string) (bool, error)
	writesChannel func(ctx context.Context, userUUID, channelID string) (bool, error)
	inGroup       func(ctx context.Context, userUUID, groupID string) (started, in bool, err error)
	inProject     func(ctx context.Context, userUUID, projectID string) (bool, error)
	readsBoard    func(ctx context.Context, userUUID, boardID string) (bool, error)
	editsBoard    func(ctx context.Context, userUUID, boardID string) (bool, error)
	readsDoc      func(ctx context.Context, userUUID, docID string) error
	addsToDoc     func(ctx context.Context, userUUID, docID string) error
}

var live = sources{
	readsChannel:  channelAnswer(channelBusiness.ReadableBy),
	writesChannel: channelAnswer(channelBusiness.WritableBy),
	inGroup:       inGroup,
	inProject:     inProject,
	readsBoard: func(ctx context.Context, userUUID, boardID string) (bool, error) {
		readable, err := boardBusiness.ReadableBoardsFor(ctx, []string{boardID}, userUUID)
		return readable[boardID], err
	},
	editsBoard: func(ctx context.Context, userUUID, boardID string) (bool, error) {
		return boardBusiness.EditableBy(ctx, boardID, userUUID)
	},
	readsDoc:  readsDoc,
	addsToDoc: addsToDoc,
}

func (s sources) mayRead(ctx context.Context, userUUID string, p Place) error {
	switch p.Kind {
	case postgresStruct.ATTACHMENT_SRC_PUBLIC:
		return nil
	case postgresStruct.ATTACHMENT_SRC_CHAT:
		return yes(inDM(userUUID, p.ID), nil)
	case postgresStruct.ATTACHMENT_SRC_GRP_CHAT:
		if !helpers.IsGroupID(p.ID) {
			return ErrRefused
		}
		started, in, err := s.inGroup(ctx, userUUID, p.ID)
		return yes(started && in, err)
	case postgresStruct.ATTACHMENT_SRC_DOC:
		if !isID(p.ID) {
			return docBusiness.ErrNotFound
		}
		return s.readsDoc(ctx, userUUID, p.ID)
	}
	ask := map[string]func(ctx context.Context, userUUID, id string) (bool, error){
		postgresStruct.ATTACHMENT_SRC_CHANNEL: s.readsChannel,
		postgresStruct.ATTACHMENT_SRC_PROJECT: s.inProject,
		postgresStruct.ATTACHMENT_SRC_BOARD:   s.readsBoard,
	}[p.Kind]
	if ask == nil || !isID(p.ID) {
		return ErrRefused
	}
	return yes(ask(ctx, userUUID, p.ID))
}

func (s sources) mayAdd(ctx context.Context, userUUID string, p Place) error {
	switch p.Kind {
	case postgresStruct.ATTACHMENT_SRC_PUBLIC:
		return nil
	case postgresStruct.ATTACHMENT_SRC_CHAT:
		return yes(inDM(userUUID, p.ID), nil)
	case postgresStruct.ATTACHMENT_SRC_GRP_CHAT:
		if !helpers.IsGroupID(p.ID) {
			return ErrRefused
		}
		started, in, err := s.inGroup(ctx, userUUID, p.ID)
		if err != nil || started {
			return yes(in, err)
		}
		return yes(namesGroup(userUUID, p.ID, p.People), nil)
	case postgresStruct.ATTACHMENT_SRC_DOC:
		if !isID(p.ID) {
			return docBusiness.ErrNotFound
		}
		return s.addsToDoc(ctx, userUUID, p.ID)
	}
	ask := map[string]func(ctx context.Context, userUUID, id string) (bool, error){
		postgresStruct.ATTACHMENT_SRC_CHANNEL: s.writesChannel,
		postgresStruct.ATTACHMENT_SRC_PROJECT: s.inProject,
		postgresStruct.ATTACHMENT_SRC_BOARD:   s.editsBoard,
	}[p.Kind]
	if ask == nil || !isID(p.ID) {
		return ErrRefused
	}
	return yes(ask(ctx, userUUID, p.ID))
}

// yes is an answer as MayRead and MayAdd give it.
func yes(ok bool, err error) error {
	switch {
	case err != nil:
		return err
	case !ok:
		return ErrRefused
	}
	return nil
}

// isID is an id as channels, projects, docs and boards are named: a uuid,
// written as they are written.
func isID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// inDM is whether the person is one of the two a direct conversation's
// grouping id names. The id names its people, so this needs no lookup, and a
// conversation not started yet is answered like any other.
func inDM(userUUID, groupingID string) bool {
	first, second, ok := helpers.DMPeople(groupingID)
	return ok && userUUID != "" && (first == userUUID || second == userUUID)
}

// namesGroup is whether people are a group's people, by its id, and include
// the person: a group not started yet is only its id until then.
func namesGroup(userUUID, groupID string, people []string) bool {
	id, err := helpers.GenerateGroupID(people)
	return err == nil && id == groupID && slices.ContainsFunc(people, func(p string) bool {
		return strings.EqualFold(strings.TrimSpace(p), userUUID)
	})
}

// channelAnswer is a channel's yes-or-no rule, with a channel that isn't
// there a no rather than an error.
func channelAnswer(rule func(ctx context.Context, userUUID, channelID string) (bool, error)) func(ctx context.Context, userUUID, channelID string) (bool, error) {
	return func(ctx context.Context, userUUID, channelID string) (bool, error) {
		ok, err := rule(ctx, userUUID, channelID)
		if errors.Is(err, channelBusiness.ErrNotFound) {
			return false, nil
		}
		return ok, err
	}
}

// inProject is projectBusiness.HasMember, the rule every project route
// keeps (controllers/ProjectAccess), with a project that isn't there a no.
func inProject(ctx context.Context, userUUID, projectID string) (bool, error) {
	ok, err := projectBusiness.HasMember(ctx, userUUID, projectID)
	if errors.Is(err, projectModels.ErrNotFound) {
		return false, nil
	}
	return ok, err
}

// inGroup is the chat rule (chatBusiness.GroupingParticipation) for the
// person with userUUID.
func inGroup(ctx context.Context, userUUID, groupID string) (started, in bool, err error) {
	person, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || person == nil {
		return false, false, err
	}
	return chatBusiness.GroupingParticipation(ctx, groupID, person.Uid)
}

// readsDoc is the doc's own answer to someone asking for its files, by its
// read rule (docBusiness.CanRead): someone it was there for is told it is
// deleted, and nobody else learns it was there.
func readsDoc(ctx context.Context, userUUID, docID string) error {
	person, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil {
		return err
	}
	if person == nil || person.Uid == "" {
		return docBusiness.ErrNotFound
	}
	doc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, docID, person.Uid)
	switch {
	case errors.Is(err, docBusiness.ErrNotFound), errors.Is(err, docBusiness.ErrDocDeleted),
		err == nil && (doc == nil || doc.Uuid == ""):
		return docBusiness.ErrNotFound
	case err != nil:
		return err
	case docBusiness.IsHosted(doc):
		// A table row's page (docs D-18): its files are its host's to show,
		// so its images show to whoever may open the row.
		return hostedDocAllows(ctx, userUUID, docID, accessBusiness.LevelView)
	case docBusiness.IsDeleted(doc) && docBusiness.CouldRead(doc, userUUID):
		return docBusiness.ErrDocDeleted
	case docBusiness.IsDeleted(doc):
		return docBusiness.ErrNotFound
	case !docBusiness.CanRead(doc, userUUID):
		return ErrRefused
	}
	return nil
}

// addsToDoc is the doc's own answer to someone adding a file to it, from the
// Access Resolver: the access a comment needs (a doc's comments take files
// this way; putting one in its body is an edit, which saving the body asks
// for). Someone it isn't shared with learns nothing, as for a doc that isn't
// there, and someone it was there for is told it is deleted. /uploadFile
// signs people in from Postgres alone, so the person is their account id.
func addsToDoc(ctx context.Context, userUUID, docID string) error {
	who := accessBusiness.Subject{UserUUID: userUUID}
	// A hosted doc (a table row's page) is decided by its host: a doc's place
	// is one of the paths that open hosted docs (business/Doc docHost.go).
	d, err := accessBusiness.Rules.Resolve(docBusiness.WithHostedOpen(ctx), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docID})
	switch {
	case errors.Is(err, accessBusiness.ErrNotFound):
		return docBusiness.ErrNotFound
	case err != nil:
		return err
	case d.Deleted && (d.Owner || d.Was >= accessBusiness.LevelView):
		return docBusiness.ErrDocDeleted
	case d.Deleted, d.Level == accessBusiness.LevelNone:
		return docBusiness.ErrNotFound
	case d.Level < accessBusiness.LevelComment:
		return ErrRefused
	}
	return nil
}

// hostedDocAllows is a hosted doc's answer to someone asking for its files,
// by its host, at least needs: as for any doc, someone it isn't there for
// learns nothing, and someone it was there for is told it is deleted.
func hostedDocAllows(ctx context.Context, userUUID, docID string, needs accessBusiness.Level) error {
	who := accessBusiness.Subject{UserUUID: userUUID}
	d, err := accessBusiness.Rules.Resolve(docBusiness.WithHostedOpen(ctx), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docID})
	switch {
	case errors.Is(err, accessBusiness.ErrNotFound):
		return docBusiness.ErrNotFound
	case err != nil:
		return err
	case d.Deleted && d.Was >= needs:
		return docBusiness.ErrDocDeleted
	case d.Deleted, d.Level < needs:
		return docBusiness.ErrNotFound
	}
	return nil
}
