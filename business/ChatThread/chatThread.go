// Package business (ChatThread) is threads you follow in chat (chat P2;
// design/chat-vs-slack/PROGRAM.md A8's core): a channel's, a DM's or a
// group's thread, who follows it, how far each has read it, and a person's
// threads, unread first.
//
// It is an index beside the replies, not a move: a chat thread is an anchored
// threads row (business/Thread's tables) whose id is its root message's uuid,
// whose resource is the conversation (kind channel, dm or group) and whose
// anchor is the message; the replies stay in the graph as they are, so search,
// the AI, imports, guests' views and live updates are unchanged.
//
// Who follows: a person follows a thread while they take part in it (they
// wrote its root, replied, or were mentioned in a reply), unless they turned
// it off; and anyone who can read it may follow it by hand. A reply makes the
// thread unread for everyone who follows it but its writer.
//
// Threads from before this are adopted as they come up: on a reply (the reply
// paths record it through business/ThreadIndex, which they can import), a
// follow or a read, and by the chat.threads.adopt sweep for those with
// replies in the last 90 days.
package business

import (
	"context"
	"errors"
	"net/http"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	domain "github.com/akashc777/OneCamp/domain/Message"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// Kinds are the conversations whose threads this follows: ThreadIndex's,
// which spells them as the Access Resolver does (kinds_test.go pins it).
var Kinds = threadIndex.Kinds

const (
	// PageSize is how many threads a page of someone's threads holds.
	PageSize = 20
	// MaxThreads is how far back someone's threads page.
	MaxThreads = 500
	// WritesPerMinute is how often one person may follow, unfollow, read or
	// mark unread their threads (registry.ThreadWriteRate).
	WritesPerMinute = 240
	// latestReplies is how many of a thread's newest replies its row reads,
	// to say how many are new (past this many, "20+").
	latestReplies = domain.MaxLatest
)

// Seams for tests.
var (
	setFollow   = models.SetFollow
	adopt       = threadIndex.Adopt
	markRead    = models.MarkRead
	markUnread  = models.MarkUnread
	markAllRead = models.MarkAllRead
	mineOf      = models.MineOf
	unreadCount = models.UnreadCount
	choiceOf    = models.ChoiceOf
	getThread   = models.Get
	resolve     = conversationBusiness.Resolve
	resolveAll  = conversationBusiness.ResolveAll
	readRoots   = domain.Messages
	readReplies = domain.RepliesOf
	allowWrite  = func(ctx context.Context, user uuid.UUID) bool {
		return redisStore.AllowFixedWindow(ctx, registry.ThreadWriteRate, []string{user.String()}, WritesPerMinute).Allowed
	}
	now = func() time.Time { return time.Now().UTC() }
)

func refuse(status int, msg string) error {
	return &sendBusiness.Rejection{Status: status, Msg: msg}
}

var (
	errNoThread = refuse(http.StatusNotFound, "That thread isn't there any more.")
	errTooFast  = refuse(http.StatusTooManyRequests, "That's a lot of changes at once. Try again in a minute.")
)

// writer is who, when they may write to their threads now.
func writer(ctx context.Context, who *userModels.UserInfo) (uuid.UUID, error) {
	user, ok := userOf(who)
	if !ok {
		return uuid.Nil, conversationBusiness.ErrNotFound
	}
	if !allowWrite(ctx, user) {
		return uuid.Nil, errTooFast
	}
	return user, nil
}

func userOf(who *userModels.UserInfo) (uuid.UUID, bool) {
	if who == nil {
		return uuid.Nil, false
	}
	if u, err := uuid.Parse(who.UserDgraphInfo.Uuid); err == nil {
		return u, true
	}
	if who.UserPostgresInfo.Id != uuid.Nil {
		return who.UserPostgresInfo.Id, true
	}
	return uuid.Nil, false
}

// rootOf finds a thread's root in the graph by its uuid: a channel's post or
// a DM's or group's message, its conversation and its author. ok is false
// for no such message, or one deleted.
func rootOf(ctx context.Context, root string) (conv conversationBusiness.Ref, by string, at time.Time, ok bool, err error) {
	// A channel's post first, then a DM's or group's message.
	if conv, by, at, ok, err = rootIn(ctx, true, root); err != nil || ok {
		return conv, by, at, ok, err
	}
	return rootIn(ctx, false, root)
}

// rootIn is rootOf for one kind of message: a channel's post when channel,
// else a DM's or group's message.
func rootIn(ctx context.Context, channel bool, root string) (conv conversationBusiness.Ref, by string, at time.Time, ok bool, err error) {
	msgs, err := readRoots(ctx, channel, []string{root})
	if err != nil {
		return conv, "", at, false, err
	}
	for _, m := range msgs {
		if m.ID != root || m.In == "" || (m.DeletedAt != nil && m.DeletedAt.Year() > 1970) {
			continue
		}
		if channel {
			conv = conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: m.In}
		} else {
			conv = conversationBusiness.ChatRef(m.In)
		}
		if m.By != nil {
			by = m.By.UUID
		}
		if m.CreatedAt != nil {
			at = *m.CreatedAt
		}
		return conv, by, at, true, nil
	}
	return conv, "", at, false, nil
}

// readable checks who may read the thread rooted at root (the Access
// Resolver's thread kind), and answers its id.
func readable(ctx context.Context, who *userModels.UserInfo, root string) (uuid.UUID, error) {
	id, err := uuid.Parse(root)
	if err != nil {
		return uuid.Nil, errNoThread
	}
	if _, err := resolve(ctx, who, conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: root}); errors.Is(err, conversationBusiness.ErrNotFound) {
		return uuid.Nil, errNoThread
	} else if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ensure makes the thread a row from its root when it isn't one yet: one
// from before threads were followed, opened or followed for the first time.
func ensure(ctx context.Context, id uuid.UUID) error {
	if row, err := getThread(ctx, id); err != nil || row != nil {
		return err
	}
	conv, by, at, ok, err := rootOf(ctx, id.String())
	if err != nil {
		return err
	}
	if !ok {
		return errNoThread
	}
	_, err = adopt(ctx, threadIndex.Thread{Kind: conv.Kind, ID: conv.ID, Root: id.String(), RootBy: by, RootAt: at})
	return err
}

// Follow sets who's own choice for the thread rooted at root: on follows it
// whatever their part, off never does. Refused in words for a thread who
// can't read, or one that isn't there.
func Follow(ctx context.Context, who *userModels.UserInfo, root string, on bool) (Choice, error) {
	user, err := writer(ctx, who)
	if err != nil {
		return Choice{}, err
	}
	id, err := readable(ctx, who, root)
	if err != nil {
		return Choice{}, err
	}
	if err := ensure(ctx, id); err != nil {
		return Choice{}, err
	}
	follow := models.FollowOff
	if on {
		follow = models.FollowOn
	}
	if err := setFollow(ctx, id, user, follow, now()); err != nil {
		return Choice{}, err
	}
	return ChoiceFor(ctx, who, root)
}

// Choice is someone's standing in a thread: whether they follow it, and why.
type Choice struct {
	Root string `json:"root"`
	// Following is whether they follow it now.
	Following bool `json:"following"`
	// Follow is their own choice: auto (while taking part), on or off.
	Follow string `json:"follow"`
}

// ChoiceFor is who's standing in the thread rooted at root.
func ChoiceFor(ctx context.Context, who *userModels.UserInfo, root string) (Choice, error) {
	user, ok := userOf(who)
	if !ok {
		return Choice{}, conversationBusiness.ErrNotFound
	}
	id, err := readable(ctx, who, root)
	if err != nil {
		return Choice{}, err
	}
	follow, follows, _, err := choiceOf(ctx, id, user)
	if err != nil {
		return Choice{}, err
	}
	return Choice{Root: root, Following: follows, Follow: follow}, nil
}

// SetRead records that who has read the thread rooted at root to now, or,
// with read false, that they haven't read its latest reply ("Mark unread").
// Nothing is kept for someone who doesn't take part in it and doesn't follow
// it.
func SetRead(ctx context.Context, who *userModels.UserInfo, root string, read bool) error {
	user, err := writer(ctx, who)
	if err != nil {
		return err
	}
	id, err := readable(ctx, who, root)
	if err != nil {
		return err
	}
	if read {
		_, err = markRead(ctx, id, user, now())
	} else {
		_, err = markUnread(ctx, id, user)
	}
	return err
}

// ReadAll records that who has read every thread they follow to its latest
// reply, and answers how many were unread.
func ReadAll(ctx context.Context, who *userModels.UserInfo) (int, error) {
	user, err := writer(ctx, who)
	if err != nil {
		return 0, err
	}
	return markAllRead(ctx, user, Kinds)
}

// UnreadCount is how many threads who follows had a reply since they last
// read them.
func UnreadCount(ctx context.Context, who *userModels.UserInfo) (int, error) {
	user, ok := userOf(who)
	if !ok {
		return 0, conversationBusiness.ErrNotFound
	}
	return unreadCount(ctx, user, Kinds)
}
