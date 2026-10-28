package business

// Telling people about words written by someone who isn't a member.
//
// A guest writes through a share link: a reply in a channel's thread, posted by
// the "Guests" principal through botpost, or a comment on a doc, kept apart
// from members' comments (business/Guest). Neither path told anyone, so a
// client's question sat unread until somebody happened to look. These reach the
// people a member's words would reach, the same ways, under the guest's name:
// a reply in a thread through NotifyThreadReply, as a member's.

import (
	"context"
	"slices"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// NotifyDocCommentFor tells the people who look after a doc (docKeepers) about
// a comment someone left on it through a share link: a push to their devices
// and an email, as a member's comment would. who is the name to show ("Priya
// (guest)"). Only keepers who may read the doc now are told, by the rule a
// member's comment asks (docCommentReadable): not its maker once it was handed
// on and they were taken off it, nor someone whose grant on it has ended. When
// that can't be said, nobody is.
func NotifyDocCommentFor(ctx context.Context, doc *dgraphStruct.DgraphDoc, who, commentID, plainText string) {
	recipients, err := keepersWhoRead(ctx, doc.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NotifyDocCommentFor could not decide who may read doc %s, so nobody is told: %+v", doc.Uuid, err)
		return
	}
	if len(recipients) == 0 {
		return
	}
	// As every push goes (Deliver): the push gate, and the desktop app's
	// notice beside the push, which a desktop-only keeper had none of.
	notificationBusiness.Deliver(ctx, recipients, map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  doc.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    "Comment - " + who,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     plainText,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: who,
	}, firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT+":"+doc.Uuid+":"+commentID)

	title := doc.Title
	if title == "" {
		title = "a document"
	}
	notificationBusiness.DispatchDocComment("", who, "", doc.Uuid, title, plainText, commentID, recipients)
}

// keepersWhoRead is the doc's keepers, read afresh as a member's comment reads
// its people (docCommentParticipants), that may read it now.
func keepersWhoRead(ctx context.Context, docUUID string) ([]string, error) {
	doc, err := docCommentParticipants(ctx, docUUID)
	if err != nil || doc == nil {
		return nil, err
	}
	readable, err := docCommentReadable(ctx, doc, nil, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range docKeepers(doc) {
		if readable(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// docKeepers is who looks after a doc: its owner (after a hand-over, the new
// one: docDomain.OwnerOf), its full-access people and its editors, each once.
// Pure.
func docKeepers(d *dgraphStruct.DgraphDoc) []string {
	if d == nil {
		return nil
	}
	var out []string
	add := func(u *dgraphStruct.DgraphUser) {
		if u != nil && u.Uuid != "" && !slices.Contains(out, u.Uuid) {
			out = append(out, u.Uuid)
		}
	}
	add(docDomain.OwnerOf(d))
	for _, list := range [][]*dgraphStruct.DgraphUser{d.FullAccessUsers, d.EditingUser} {
		for _, u := range list {
			add(u)
		}
	}
	return out
}
