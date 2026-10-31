package business

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	"github.com/google/uuid"
)

// CallThread is a call's thread as someone outside its conversation reads it
// (calls CALLS-3b, agreed with CHAT-LEAD-2): the call's post and the replies
// written while the call went on, as plain text with their writers' names.
// Nothing of the conversation: not its name, its id or its people.
type CallThread struct {
	Post    GuestMessage  `json:"post"`
	Replies []CallMessage `json:"replies"`
	// CanReply is whether the reader may write in it: business/CallThread's
	// to say.
	CanReply bool `json:"can_reply"`
	// ChatHeld is why they may not: the people running the call hold chat for
	// everyone else (calls meet-12).
	ChatHeld bool `json:"chat_held,omitempty"`
	// Latest is what the app sends back as a poll's since: the newest reply's
	// time as the thread's index has it, or the post's own when it has none.
	// business/CallThread's to say.
	Latest time.Time `json:"latest"`
}

// CallMessage is a reply in a call's thread as everyone in the call reads it.
type CallMessage struct {
	GuestMessage
	// AuthorKey is the same on one member's replies in this thread and means
	// nothing outside it, so the call's chat runs a person's replies together
	// without any member's id leaving the server. A bot's replies have none:
	// a relay speaks for many people, and a name isn't a person (two guests
	// may both be "Guest").
	AuthorKey string `json:"author_key,omitempty"`
	// Guest is a guest's reply, its Author the name they gave, which the
	// call marks as a guest's as its tiles do.
	Guest bool `json:"guest,omitempty"`
}

// ReplyInCallThread writes a guest's reply in a channel's call thread (post,
// in channel): as the guest bot, under their name, as a guest's reply in
// their own channel is written, and told to the thread's people alike. Who
// may is business/CallThread's to decide (a guest in the live call). A
// channel that's archived or gone takes nothing.
func ReplyInCallThread(ctx context.Context, post, channel, name, text string) error {
	channelID, err := uuid.Parse(channel)
	if err != nil {
		return ErrNotFound
	}
	body, err := GuestMessageHTML(name, text)
	if err != nil {
		return err
	}
	bot, err := userBusiness.EnsureGuestBot(ctx)
	if err != nil {
		return err
	}
	if _, err := liveChannel(ctx, channelID, bot); err != nil {
		return err
	}
	return replyAsGuest(ctx, bot, channelID, post, body, guestLabel(name), strings.TrimSpace(text))
}

// GetCallThread reads the call's thread over [from, until): post is the
// call's post, in a conversation of kind (channel, dm or group). Who may
// read it is business/CallThread's to decide.
func GetCallThread(ctx context.Context, kind, post string, from, until time.Time) (*CallThread, error) {
	var root GuestMessage
	var replies []*dgraphStruct.DgraphComment
	bot, err := userBusiness.EnsureGuestBot(ctx)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "channel":
		p, err := postDomain.GetDgraphPostByUUIDWithAllComments(ctx, post, bot.DgraphUID)
		if readFailed(err, postModels.ErrNotFound) {
			return nil, err
		}
		if p == nil || p.Uuid == "" || helpers.IsSoftDeleted(p.DeletedAt) {
			return nil, ErrNotFound
		}
		root, replies = guestMessage(p.Uuid, p.PostBy, p.Text, p.CreatedAt), p.Comments
	case "dm", "group":
		c, err := chatDomain.GetDgraphChatByUUIDWithAllComments(ctx, post)
		if readFailed(err, chatModels.ErrNotFound) {
			return nil, err
		}
		if c == nil || c.Uuid == "" || helpers.IsSoftDeleted(c.DeletedAt) {
			return nil, ErrNotFound
		}
		root, replies = guestMessage(c.Uuid, c.From, c.Body, c.CreatedAt), c.Comments
	default:
		return nil, ErrNotFound
	}
	t := &CallThread{Post: root, Replies: callSpan(replies, from, until, post, bot.UUID)}
	t.Post.ReplyCount = uint64(len(t.Replies))
	return t, nil
}

// callSpan is the replies to post written within [from, until), oldest
// first, the deleted and the undated left out (CHAT-LEAD (c)); guestBot is
// who writes a guest's. Pure.
func callSpan(replies []*dgraphStruct.DgraphComment, from, until time.Time, post, guestBot string) []CallMessage {
	kept := make([]*dgraphStruct.DgraphComment, 0, len(replies))
	for _, c := range replies {
		if c == nil || c.CreatedAt == nil || helpers.IsSoftDeleted(c.DeletedAt) || c.CreatedAt.Before(from) || !c.CreatedAt.Before(until) {
			continue
		}
		kept = append(kept, c)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].CreatedAt.Before(*kept[j].CreatedAt) })
	out := make([]CallMessage, 0, len(kept))
	for _, c := range kept {
		m := CallMessage{GuestMessage: guestMessage(c.Uuid, c.CommentBy, c.Text, c.CreatedAt)}
		switch by := c.CommentBy; {
		case by == nil:
		case by.Uuid == guestBot:
			m.Author, m.Guest = guestNameOf(m.Author), true
		case !by.IsBot && by.Uuid != "":
			m.AuthorKey = authorKey(post, by.Uuid)
		}
		out = append(out, m)
	}
	return out
}

// authorKey is who wrote a reply to post: the same for one person's replies
// there, and nothing anyone can read an id from. Pure.
func authorKey(post, user string) string {
	sum := sha256.Sum256([]byte(post + "/" + user))
	return hex.EncodeToString(sum[:8])
}
