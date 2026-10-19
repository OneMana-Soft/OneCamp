package business

// Changes to a comment already written, on a kind whose comments are stored
// here (StoredComments): its writer edits it; its writer removes it while they
// can still read where it is, and a moderator (the resource's owner, someone
// with full access, a workspace admin acting themselves) removes anyone's, by
// the one rule doc comments use (business/Comment CanRemoveComment); and anyone
// who may comment reacts, one reaction a person. A doc's comments change
// through the doc's own comment routes, as before: here they are unavailable.
//
// Each change is a Resource Event, as every thread change is (events.go), so
// open readers hear of it; an event carries ids, never a comment's text.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

// The kinds of a stored comment's events. Never renamed once shipped.
const (
	ThreadCommentEdited  resourceEventBusiness.Kind = "thread.comment.edited"
	ThreadCommentRemoved resourceEventBusiness.Kind = "thread.comment.removed"
	ThreadCommentReacted resourceEventBusiness.Kind = "thread.comment.reacted"
)

// Removed says how a comment was removed: by someone other than its writer
// (Moderated), and whether only being a workspace admin allowed that (ByAdmin:
// an admin act, which the caller records in the audit log).
type Removed struct {
	Moderated bool
	ByAdmin   bool
}

// CommentChanges are the changes to a stored comment.
type CommentChanges interface {
	// Edit gives a comment its writer's new words.
	Edit(ctx context.Context, who accessBusiness.Subject, threadID, commentID string, body Body) (Comment, error)
	// Remove takes a comment from every reader.
	Remove(ctx context.Context, who accessBusiness.Subject, threadID, commentID string) (Removed, error)
	// React puts who's reaction on a comment, or changes its emoji; it
	// answers the reaction's id.
	React(ctx context.Context, who accessBusiness.Subject, threadID, commentID, emojiID string) (string, error)
	// Unreact takes back who's own reaction.
	Unreact(ctx context.Context, who accessBusiness.Subject, threadID, commentID, reactionID string) error
}

// Comments is the one place a stored comment changes.
var Comments CommentChanges = commentChanges{s: Threads.(*service)}

// Seams for tests: where a stored comment is read and changed.
var (
	commentGet      = models.GetComment
	commentEdit     = models.EditComment
	commentRemove   = models.RemoveComment
	reactionSet     = models.SetReaction
	reactionRemove  = models.RemoveReaction
	reactionsOfOne  = models.ReactionsOf
	commentsNewUUID = uuid.New
)

// storesComments is a Resource whose comments are stored here: one that
// embeds StoredComments.
type storesComments interface {
	storedKind() string
}

func (s StoredComments) storedKind() string { return s.Kind }

type commentChanges struct{ s *service }

// target is the thread, the comment in it and who's decision there, cleared
// for a: a thread on a kind that stores its comments here, and a comment that
// is live and in that thread. Anything else is not found, as the thread's own
// routes answer for something the person can't see.
func (c commentChanges) target(ctx context.Context, who accessBusiness.Subject, threadID, commentID string, a act) (Thread, *models.Comment, accessBusiness.Decision, error) {
	t, r, err := c.s.load(ctx, threadID)
	if err != nil {
		return Thread{}, nil, accessBusiness.Decision{}, err
	}
	if _, stored := r.(storesComments); !stored || t.legacy {
		return Thread{}, nil, accessBusiness.Decision{}, ErrUnavailable
	}
	d, err := r.Access(ctx, who, resOf(t.Thread))
	if err != nil {
		return Thread{}, nil, d, err
	}
	if err := allowed(d, a, false); err != nil {
		return Thread{}, nil, d, err
	}
	id, err := uuid.Parse(commentID)
	if err != nil {
		return Thread{}, nil, d, ErrNotFound
	}
	row, err := commentGet(ctx, id)
	if err != nil {
		return Thread{}, nil, d, err
	}
	if row == nil || row.DeletedAt != nil || row.ThreadID.String() != t.ID {
		return Thread{}, nil, d, ErrNotFound
	}
	return t.Thread, row, d, nil
}

func (c commentChanges) Edit(ctx context.Context, who accessBusiness.Subject, threadID, commentID string, body Body) (Comment, error) {
	if _, err := validBody(body); err != nil {
		return Comment{}, err
	}
	t, row, _, err := c.target(ctx, who, threadID, commentID, actPost)
	if err != nil {
		return Comment{}, err
	}
	if !strings.EqualFold(row.AuthorUUID.String(), who.UserUUID) {
		return Comment{}, &Refusal{Msg: "Only the person who wrote a comment can edit it."}
	}
	html, err := safeBody(body.HTML)
	if err != nil {
		return Comment{}, err
	}
	at := c.s.now()
	ok, err := commentEdit(ctx, row.ID, html, at)
	if err != nil {
		return Comment{}, err
	}
	if !ok {
		return Comment{}, ErrNotFound // removed while it was being edited
	}
	row.Body, row.EditedAt = html, &at
	c.s.emit(ctx, ThreadCommentEdited, t, who, nil, row.ID.String())
	reactions, err := reactionsOfOne(ctx, []uuid.UUID{row.ID})
	if err != nil {
		reactions = nil
	}
	names, err := storedPeople(ctx, []string{row.AuthorUUID.String()})
	if err != nil {
		names = nil
	}
	return storedComment(row, reactions[row.ID], names), nil
}

func (c commentChanges) Remove(ctx context.Context, who accessBusiness.Subject, threadID, commentID string) (Removed, error) {
	t, row, d, err := c.target(ctx, who, threadID, commentID, actRead)
	if err != nil {
		return Removed{}, err
	}
	author := strings.EqualFold(row.AuthorUUID.String(), who.UserUUID)
	if !commentBusiness.CanRemoveComment(author, d.Level >= accessBusiness.LevelView, moderatorOf(ctx, who, d)) {
		return Removed{}, &Refusal{Msg: "Only the person who wrote a comment, or someone who looks after this, can delete it."}
	}
	by, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return Removed{}, ErrForbidden
	}
	ok, err := commentRemove(ctx, row.ID, by, c.s.now())
	if err != nil {
		return Removed{}, err
	}
	if !ok {
		return Removed{}, ErrNotFound
	}
	c.s.emit(ctx, ThreadCommentRemoved, t, who, nil, row.ID.String())
	out := Removed{Moderated: !author}
	// Only an admin's own say allowed it: not the owner, and full access
	// only as an admin (the resource's rule names why, as "admin").
	out.ByAdmin = out.Moderated && !d.Owner && onlyAsAdmin(d)
	return out, nil
}

func (c commentChanges) React(ctx context.Context, who accessBusiness.Subject, threadID, commentID, emojiID string) (string, error) {
	emojiID, ok := helpers.EmojiID(emojiID)
	if !ok {
		return "", &Invalid{Msg: "Pick an emoji to react with."}
	}
	t, row, _, err := c.target(ctx, who, threadID, commentID, actPost)
	if err != nil {
		return "", err
	}
	user, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return "", ErrForbidden
	}
	id, ok, err := reactionSet(ctx, commentsNewUUID(), row.ID, user, emojiID, c.s.now())
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	c.s.emit(ctx, ThreadCommentReacted, t, who, nil, row.ID.String())
	return id.String(), nil
}

func (c commentChanges) Unreact(ctx context.Context, who accessBusiness.Subject, threadID, commentID, reactionID string) error {
	t, row, _, err := c.target(ctx, who, threadID, commentID, actRead)
	if err != nil {
		return err
	}
	user, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return ErrForbidden
	}
	rid, err := uuid.Parse(reactionID)
	if err != nil {
		return ErrNotFound
	}
	ok, err := reactionRemove(ctx, row.ID, rid, user)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	c.s.emit(ctx, ThreadCommentReacted, t, who, nil, row.ID.String())
	return nil
}

// onlyAsAdmin is whether a decision's level comes from being a workspace
// admin and nothing else the resource gives. Pure.
func onlyAsAdmin(d accessBusiness.Decision) bool {
	if len(d.Reasons) == 0 {
		return false
	}
	for _, r := range d.Reasons {
		if r != "admin" {
			return false
		}
	}
	return true
}
