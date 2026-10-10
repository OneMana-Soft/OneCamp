package business

// Telling people about words written by someone who isn't a member.
//
// A guest writes through a share link: a reply in a channel's thread, posted by
// the "Guests" principal through botpost, or a comment on a doc, kept apart
// from members' comments (business/Guest). Neither path told anyone, so a
// client's question sat unread until somebody happened to look. These reach the
// people a member's words would reach, the same ways, under the guest's name.

import (
	"context"
	"fmt"
	"slices"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// NotifyPostCommentFor tells a thread's people about a reply a principal posted
// for someone (a guest's, through botpost), as a member's reply tells them
// (sendNewPostCommentNotification): the post's author and those who replied
// before, by push and email, each as their channel setting allows. by is the
// principal, named as whom it wrote for, so it is never told of its own words.
func NotifyPostCommentFor(by *dgraphStruct.DgraphUser, post *dgraphStruct.DgraphPost, commentUUID, plainText string) {
	if by == nil || post == nil || post.PostBy == nil || post.Channel == nil {
		return
	}
	sendNewPostCommentNotification(fmt.Sprintf("Comment - %+v", by.DisplayName()), plainText, commentUUID, nil, post, by)
}

// NotifyDocCommentFor tells whoever made a doc, and those who can edit it,
// about a comment someone left on it through a share link: a push to their
// devices and an email, as a member's comment would. who is the name to show
// ("Priya (guest)").
func NotifyDocCommentFor(ctx context.Context, doc *dgraphStruct.DgraphDoc, who, commentID, plainText string) {
	recipients := docKeepers(doc)
	if len(recipients) == 0 {
		return
	}
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, recipients)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NotifyDocCommentFor Failed to get fcm tokens err: %+v", err)
	}
	pushData := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  doc.Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    "Comment - " + who,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     plainText,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: who,
	}
	// 500 tokens a call is Firebase's limit.
	for batch := range slices.Chunk(tokens, 500) {
		if err := firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, batch); err != nil {
			helpers.LogErrorWithContext(ctx, "business/NotifyDocCommentFor Failed to send push notification err: %+v", err)
			break
		}
	}

	title := doc.Title
	if title == "" {
		title = "a document"
	}
	notificationBusiness.DispatchDocComment("", who, "", doc.Uuid, title, plainText, commentID, recipients)
}

// docKeepers is who looks after a doc: whoever made it and those who can edit
// it, each once. Pure.
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
	add(d.CreatedBy)
	for _, u := range d.EditingUser {
		add(u)
	}
	return out
}
