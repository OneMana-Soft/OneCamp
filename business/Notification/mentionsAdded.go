package notification

// Telling whom a save newly mentions (NOTIF-H3).
//
// A task's words and a message's or a comment's keep who they mention on one
// mention node, which every save rewrites in place. A send told the people it
// named; an edit told nobody, and the person it added was listed at the first
// save's time, under their last look. Now each save that names someone the
// node didn't tells them, once ever: the inbox notice is keyed by the thing and
// the person (followModels.MentionKey), so a race, a re-save, or a remove and a
// re-add tells nobody twice, and the feed lists the notice in place of the
// graph's mention (business/Activity's feedPage), at the save that named them.

import (
	"context"

	activityDomain "github.com/akashc777/OneCamp/domain/Activity"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// Seam for tests: who a thing's mention node names now.
var mentionedNow = activityDomain.Mentioned

// The notice kinds of a mention a save added: in a task's words or one of
// its comments (ResourceKind "task"), and in a channel's or a conversation's
// message or one of its replies (ResourceKind "channel" or "conversation",
// whose packages register them).
const (
	TaskMentioned    = "task.mentioned"
	MessageMentioned = "message.mentioned"
)

// MentionSave is a save of a thing's words: who its mention node named
// before it and who it names now (user uuids).
type MentionSave struct {
	// Kind is what holds the mention node ("task", "post", "chat",
	// "comment"), and ID its uuid.
	Kind, ID      string
	Before, After []string
	// Hear is who of the people the save adds hears of a mention there, by
	// their setting, and may read it now. An answer that can't be read tells
	// nobody.
	Hear func(ctx context.Context, added []string) ([]string, error)
	// Named is who of them the words name themselves; the rest were reached
	// by a broadcast or a team, and aren't emailed, as a send doesn't email
	// them. Nil: all of them are named.
	Named func(userUUID string) bool
}

// SaveOfMentions is a save of a thing's words that names after, read before
// its write: who the thing's mention node names now. ok is false when the save
// can tell nobody: an import's, one that names nobody (nothing is read then),
// or one whose node couldn't be read.
func SaveOfMentions(ctx context.Context, kind, id string, after []*dgraphStruct.DgraphUser) (s MentionSave, ok bool) {
	s = MentionSave{Kind: kind, ID: id}
	if helpers.IsBulkImport(ctx) {
		return s, false // an import's save: nobody wrote its mentions just now
	}
	for _, u := range after {
		if u != nil && u.Uuid != "" {
			s.After = append(s.After, u.Uuid)
		}
	}
	if len(s.After) == 0 {
		return s, false
	}
	before, err := mentionedNow(ctx, kind, id)
	if err != nil {
		return s, false
	}
	s.Before = before
	return s, true
}

// ActorOf is who saved, by uuid, as a notice names them: the person the
// request is from when it is them (their names), else their id alone (a
// bridge's or a copy's save, made for them).
func ActorOf(ctx context.Context, userUUID string) *dgraphStruct.DgraphUser {
	if u, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo); ok && u.UserDgraphInfo.Uuid == userUUID {
		return &u.UserDgraphInfo
	}
	return &dgraphStruct.DgraphUser{Uuid: userUUID}
}

// MentionsAdded is whom after names that before didn't, never the actor, each
// once, in after's order. Pure.
func MentionsAdded(before, after []string, actor string) []string {
	had := map[string]bool{"": true, actor: true}
	for _, id := range before {
		had[id] = true
	}
	var added []string
	for _, id := range after {
		if !had[id] {
			had[id] = true
			added = append(added, id)
		}
	}
	return added
}

// TellMentionsAdded tells each person the save newly names (MentionsAdded)
// who hears of it (s.Hear), once ever, as n reads: in the inbox, by push, and
// by email when the words name them (s.Named). n's DedupKey is the mention's
// key. Answers who was told.
func TellMentionsAdded(ctx context.Context, n Notice, s MentionSave) []string {
	actor := ""
	if n.Actor != nil {
		actor = n.Actor.Uuid
	}
	added := MentionsAdded(s.Before, s.After, actor)
	if len(added) == 0 {
		return nil
	}
	hear, err := s.Hear(ctx, added)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/TellMentionsAdded %s %s: who hears of it couldn't be read, telling nobody: %+v", s.Kind, s.ID, err)
		return nil
	}
	isAdded := make(map[string]bool, len(added))
	for _, id := range added {
		isAdded[id] = true
	}
	to := make([]NoticeRecipient, 0, len(hear))
	for _, id := range hear {
		if isAdded[id] {
			to = append(to, mentioned(id, s))
		}
	}
	return tellMentioned(ctx, n, s, to)
}

// NoteMentionsAdded tells, of a mention a save added that went through
// business/Notify's door (Plan chose who hears of it, its rows were written
// and its push delivered: an edit of a chat message, V-843), the rest as
// TellMentionsAdded tells it, once ever: the inbox notice of each of inbox,
// and the email of each of emailed whom the words name (s.Named). Never a
// push; s.Hear isn't asked. Answers who was told.
func NoteMentionsAdded(ctx context.Context, n Notice, s MentionSave, inbox, emailed []string) []string {
	mail := make(map[string]bool, len(emailed))
	for _, id := range emailed {
		mail[id] = true
	}
	to := make([]NoticeRecipient, 0, len(inbox))
	for _, id := range inbox {
		r := mentioned(id, s)
		r.NoPush, r.NoEmail = true, r.NoEmail || !mail[id]
		to = append(to, r)
	}
	return tellMentioned(ctx, n, s, to)
}

// mentioned is id, whom a save newly names, as a notice tells them: by email
// only when the words name them (s.Named). Pure.
func mentioned(id string, s MentionSave) NoticeRecipient {
	return NoticeRecipient{UserUUID: id, Reason: "mention", NoEmail: s.Named != nil && !s.Named(id)}
}

// RetractMentionNotices takes back the inbox notices of the mentions in the
// message messageID (a post, chat or comment uuid) that people were told of
// (soft: followModels.RetractNotices), when the message is deleted. people is
// whom its notices may have reached: whose deliveries of it were taken back
// (business/Notify Retract), since a notice of a message's mention goes only
// to those the door told of it (NoteMentionsAdded). One indexed write.
func RetractMentionNotices(ctx context.Context, messageID string, people []string) error {
	keys := make([]string, 0, len(people)*len(messageKinds))
	for _, kind := range messageKinds {
		key := followModels.MentionKey(kind, messageID)
		for _, p := range people {
			keys = append(keys, key+"|"+p)
		}
	}
	_, err := retractNotices(ctx, keys)
	return err
}

// messageKinds is what holds a message's mention node (MentionSave.Kind): a
// channel post, a DM or group message, and a reply to either.
var messageKinds = []string{"post", "chat", "comment"}

// Seam for tests: the soft take-back of notices by key.
var retractNotices = followModels.RetractNotices

// tellMentioned tells to of the mention s added, as n reads, keyed by the
// mention (followModels.MentionKey), so nobody is told of it twice.
func tellMentioned(ctx context.Context, n Notice, s MentionSave, to []NoticeRecipient) []string {
	n.DedupKey = followModels.MentionKey(s.Kind, s.ID)
	return Tell(ctx, n, to)
}
