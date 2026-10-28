package business

// Who hears about a new comment on a doc (D-13), and the bus sink that tells
// them.
//
// A member's comment emits doc.comment.created (CreateCommentInADoc), and this
// sink is the one notifier: it reads the comment and its author back and runs
// sendNewDocCommentNotification, which pushes, emails and writes inbox items,
// each way on its own (B4). Nobody else sends anything for the comment.
//
// THE RULE. The doc's participants, as W0-3 fixed it: its owner, its editors,
// whoever commented on it before, and anyone the comment mentions, each once,
// never its author, and only people who may read the doc now. Its readers who
// took no part aren't told of every comment, so ten readers and one commenter
// means the owner and the commenter hear of the next comment, nobody else.
// Follows go on top: someone who chose "none" isn't told as a participant (a
// mention still reaches them, as a mention always does), and someone who chose
// "all" is told of every comment they may read. "Replies and mentions", the
// default for the owner and editors, is the participants rule itself.
//
// THE INBOX. The feed lists a comment on what you made, and a mention of you,
// from the graph already. Everyone else told (an editor, an earlier commenter,
// a follower) gets an inbox notice, so the comment isn't only a push.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Comment"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

// DocReaders answers, of the people named (by uuid), who may read a doc now,
// keyed as given: the Access Resolver's rule (business/Doc Readers), which
// counts the doc's teams, its space and the grants that have ended. An error
// says nothing about anyone.
type DocReaders func(ctx context.Context, docUUID string, userUUIDs []string) (map[string]bool, error)

// docReaders is the doc's read rule. business/Doc imports this package, so it
// hands the rule over when it starts (RegisterDocReaders). Until it has, a
// comment tells nobody: a comment unheard of costs less than a private doc's
// comment told to someone who can't read it.
var docReaders DocReaders

// errNoReadRule is a comment told before the doc's read rule was handed over.
var errNoReadRule = errors.New("comment: no read rule for docs yet")

// RegisterDocReaders hands the comment notices the doc's read rule; business/Doc
// does when it starts.
func RegisterDocReaders(readers DocReaders) { docReaders = readers }

// Seams for tests: who follows a doc, and where its inbox notices go.
var (
	docCommentFollows = func(ctx context.Context, docUUID string) (map[string]string, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return nil, nil
		}
		return followModels.FollowModes(ctx, "doc", docUUID)
	}
	docCommentInbox = func(ctx context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) {
		notificationBusiness.Tell(ctx, n, to)
	}
	// What the sink reads back: the author, and the comment's words.
	docCommentAuthor = userDomain.GetActiveDgraphUserInfoByUUID
	docCommentText   = func(ctx context.Context, commentUUID string) (string, error) {
		text, _, err := domain.GetCommentForTelling(ctx, commentUUID)
		return text, err
	}
)

// ThreadReplyKey marks a doc comment posted as a reply in a thread: the
// thread's code (business/Thread) posts it with the thread's id on its
// context, context.WithValue(ctx, ThreadReplyKey, threadID). The thread's own
// event (thread.replied) then tells the thread's people and the doc's
// followers, so the doc-wide notice for the comment stops and nobody hears
// twice (A8, D-13). A thread's first comment is told doc-wide, as any comment.
var ThreadReplyKey = threadReply{}

type threadReply struct{}

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "doc.comment.notify",
		Wants: wantsDocComment,
		Run:   notifyDocComment,
	})
}

// wantsDocComment is a comment the sink tells doc-wide: any comment on a doc
// but a reply in a thread, which the thread's event tells.
func wantsDocComment(e resourceEventBusiness.Event) bool {
	return e.Kind == resourceEventBusiness.DocCommentCreated && e.ResourceKind == "doc" && e.Data["thread_reply"] != "true"
}

// emitDocComment is a member's new comment on a doc, as a Resource Event: who
// wrote it, and whom it mentions. Ids only; the sink reads the words back.
func emitDocComment(ctx context.Context, docUUID, authorUUID, commentUUID string, at time.Time, mentions []*dgraphStruct.DgraphUser) {
	mentioned := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m != nil && m.Uuid != "" {
			mentioned = append(mentioned, m.Uuid)
		}
	}
	data := map[string]string{"comment_uuid": commentUUID}
	if thread, _ := ctx.Value(ThreadReplyKey).(string); thread != "" {
		data["thread_id"], data["thread_reply"] = thread, "true"
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind:         resourceEventBusiness.DocCommentCreated,
		ResourceKind: "doc",
		ResourceUUID: docUUID,
		Actor:        resourceEventBusiness.Actor{UserUUID: authorUUID},
		At:           at,
		Subjects:     mentioned,
		Data:         data,
	})
}

// notifyDocComment is the sink: the comment's author and words, read back, and
// everyone it is for told.
func notifyDocComment(ctx context.Context, e resourceEventBusiness.Event) {
	commentUUID := e.Data["comment_uuid"]
	if e.Actor.UserUUID == "" || commentUUID == "" {
		return
	}
	author, err := docCommentAuthor(ctx, e.Actor.UserUUID)
	if err != nil || author == nil {
		helpers.LogErrorWithContext(ctx, "business/notifyDocComment Failed to read the author of comment %s err: %+v", commentUUID, err)
		return
	}
	text, err := docCommentText(ctx, commentUUID)
	if err != nil {
		// The words couldn't be read: tell people anyway, without them.
		helpers.LogErrorWithContext(ctx, "business/notifyDocComment Failed to read comment %s err: %+v", commentUUID, err)
	}
	mentions := make([]*dgraphStruct.DgraphUser, 0, len(e.Subjects))
	for _, id := range e.Subjects {
		mentions = append(mentions, &dgraphStruct.DgraphUser{Uuid: id})
	}
	sendNewDocCommentNotification(fmt.Sprintf("Comment - %+v", author.DisplayName()), helpers.HTMLToPlainText(text),
		commentUUID, &dgraphStruct.DgraphDoc{Uuid: e.ResourceUUID}, author, mentions)
}

// docCommentRecipients is who hears about a new comment on a doc: its owner,
// its full-access people and editors, whoever commented on it before, and anyone the comment
// mentions, in that order and each once, never its author, and only people
// who may read the doc now (readable: the doc's read rule, docReaders). Its
// readers who haven't taken part aren't told of every comment. doc is read as
// GetDocCommentParticipants reads it. Pure.
func docCommentRecipients(doc *dgraphStruct.DgraphDoc, mentioned []string, author string, readable func(string) bool) []string {
	if doc == nil || readable == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{author: true, "": true}
	add := func(id string) {
		if !seen[id] && readable(id) {
			out = append(out, id)
		}
		seen[id] = true
	}
	for _, id := range takingPart(doc) {
		add(id)
	}
	for _, id := range mentioned {
		add(id)
	}
	return out
}

// takingPart is who takes part in a doc, in order: its keepers (docKeepers:
// its owner, its full-access people and editors) and whoever commented on it.
// Pure.
func takingPart(doc *dgraphStruct.DgraphDoc) []string {
	out := docKeepers(doc)
	for _, c := range doc.Comments {
		if c != nil && c.CommentBy != nil {
			out = append(out, c.CommentBy.Uuid)
		}
	}
	return out
}

// docCommentAudience is docCommentRecipients with the doc's follows on top
// (follows: each person's choice, "all", "mentions" or "none"; nobody's made
// is nil): a participant who chose "none" is left out unless the comment
// mentions them, and then whoever chose "all" and may read the doc, in a
// stable order. Pure.
func docCommentAudience(doc *dgraphStruct.DgraphDoc, mentioned []string, author string, follows map[string]string, readable func(string) bool) []string {
	isMentioned := map[string]bool{}
	for _, id := range mentioned {
		isMentioned[id] = true
	}
	var out []string
	seen := map[string]bool{author: true, "": true}
	for _, id := range docCommentRecipients(doc, mentioned, author, readable) {
		seen[id] = true
		if follows[id] == followModels.ModeNone && !isMentioned[id] {
			continue
		}
		out = append(out, id)
	}
	var all []string
	for id, mode := range follows {
		if mode == followModels.ModeAll && !seen[id] {
			all = append(all, id)
		}
	}
	sort.Strings(all)
	for _, id := range all {
		if readable(id) {
			out = append(out, id)
		}
	}
	return out
}

// docCommentCandidates is everyone a comment on the doc could be told to: its
// owner, its editors, whoever commented on it before, the people it mentions
// and whoever follows the doc all, each once, for the doc's read rule to be
// asked about at once. Pure.
func docCommentCandidates(doc *dgraphStruct.DgraphDoc, mentioned []string, follows map[string]string) []string {
	var out []string
	seen := map[string]bool{"": true}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range takingPart(doc) {
		add(id)
	}
	for _, id := range mentioned {
		add(id)
	}
	var all []string
	for id, mode := range follows {
		if mode == followModels.ModeAll {
			all = append(all, id)
		}
	}
	sort.Strings(all)
	for _, id := range all {
		add(id)
	}
	return out
}

// docCommentReadable asks the doc's read rule, once, about everyone a comment
// on it could be told to (docCommentCandidates), and answers who may read it:
// anyone not asked about may not. An error, or no rule yet, tells nobody.
func docCommentReadable(ctx context.Context, doc *dgraphStruct.DgraphDoc, mentioned []string, follows map[string]string) (func(string) bool, error) {
	if docReaders == nil {
		return nil, errNoReadRule
	}
	can, err := docReaders(ctx, doc.Uuid, docCommentCandidates(doc, mentioned, follows))
	if err != nil {
		return nil, err
	}
	return func(id string) bool { return can[id] }, nil
}

// tellDocCommentInbox writes the inbox notices for a comment: for everyone
// told but its doc's maker and the people it mentions, whose feed lists it
// already (the feed lists a comment on what someone made, so after a
// hand-over the new owner gets an inbox item and the maker doesn't). readable is the doc's read rule, as the recipients were chosen by.
func tellDocCommentInbox(ctx context.Context, doc *dgraphStruct.DgraphDoc, author *dgraphStruct.DgraphUser, commentUUID string, mentioned, recipients []string, follows map[string]string, readable func(string) bool) {
	listed := map[string]bool{}
	if doc.CreatedBy != nil {
		listed[doc.CreatedBy.Uuid] = true
	}
	for _, id := range mentioned {
		listed[id] = true
	}
	takesPart := map[string]bool{}
	for _, id := range docCommentRecipients(doc, nil, author.Uuid, readable) {
		takesPart[id] = true
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range recipients {
		if listed[id] {
			continue
		}
		reason := "participant"
		if !takesPart[id] && follows[id] == followModels.ModeAll {
			reason = "follower"
		}
		to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: reason, InboxOnly: true})
	}
	if len(to) == 0 {
		return
	}
	title := doc.Title
	if title == "" {
		title = "a document"
	}
	docCommentInbox(ctx, notificationBusiness.Notice{
		Kind:          string(resourceEventBusiness.DocCommentCreated),
		ResourceKind:  "doc",
		ResourceUUID:  doc.Uuid,
		ResourceTitle: doc.Title,
		Actor:         author,
		Data:          map[string]string{"comment_uuid": commentUUID},
		// Keyed by the comment, as a thread's notices are: one inbox item
		// for one comment, whichever way it was told.
		DedupKey: "comment|" + commentUUID,
		Title:    author.DisplayName() + " commented on " + title,
		Path:     "/app/doc/" + doc.Uuid,
	}, to)
}
