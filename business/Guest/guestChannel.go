package business

// Channel guests: Slack Connect's job for a self-hosted workspace. A channel
// admin gives someone outside the workspace a link to one channel; they read
// it and (with the post capability) write in it, threads included.
//
// THE SAME CONSTRUCTION AS EVERY OTHER GUEST LINK. A guest is a grant, never a
// user: they have no session, so no ordinary endpoint can serve them, and they
// cannot appear in rosters, search, memory or mentions. What they write is
// posted by one principal, "Guests", with each message led by the guest's
// name, the way the Slack bridge carries Slack people. They see the channel's
// messages as plain text, with names, and nothing else of the workspace.

import (
	"context"
	"errors"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// ErrGuestInput is a message the guest can fix; its text is written for them.
type ErrGuestInput struct{ Msg string }

func (e *ErrGuestInput) Error() string { return e.Msg }

const maxGuestMessage = 4000

// ErrNotFound is a message outside the guest's channel, or gone; both answer
// the same.
var ErrNotFound = errors.New("not found")

// GuestMessage is one message as a guest sees it.
type GuestMessage struct {
	ID         string    `json:"id"`
	Author     string    `json:"author"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
	ReplyCount uint64    `json:"reply_count"`
}

// GuestChannelView is a page of a channel for its guest.
type GuestChannelView struct {
	Channel  string         `json:"channel"`
	CanPost  bool           `json:"can_post"`
	Messages []GuestMessage `json:"messages"`
	HasMore  bool           `json:"has_more"`
}

func authorName(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return "Someone"
	}
	if u.UserFullName != "" {
		return u.UserFullName
	}
	if u.UserName != "" {
		return u.UserName
	}
	return "Someone"
}

// PlainText is a stored message as plain text for a guest: tags gone,
// entities decoded, whitespace tidy. Pure.
func PlainText(stored string) string {
	t := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n").Replace(stored)
	t = html.UnescapeString(helpers.RemoveHTMLTags(t))
	lines := strings.Split(t, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func channelOf(grant *guestModel.GuestGrant) (uuid.UUID, error) {
	if grant.ResourceType != guestModel.ResourceChannel {
		return uuid.Nil, ErrForbidden
	}
	return uuid.Parse(grant.ResourceID)
}

// GetGuestChannel is the channel's messages before a moment (now when zero),
// newest first, a page at a time.
func GetGuestChannel(ctx context.Context, grant *guestModel.GuestGrant, before time.Time) (*GuestChannelView, error) {
	channelID, err := channelOf(grant)
	if err != nil {
		return nil, err
	}
	bot, err := userBusiness.EnsureChannelGuestBot(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelID.String(), bot.DgraphUID)
	if err != nil || ch == nil || ch.Uuid == "" || helpers.IsSoftDeleted(ch.DeletedAt) {
		return nil, ErrNotFound
	}
	if before.IsZero() {
		before = time.Now()
	}
	posts, err := postDomain.GetDgraphOldPostFromDgraph(ctx, channelID.String(), before)
	if err != nil {
		return nil, err
	}
	view := &GuestChannelView{Channel: ch.Name, CanPost: grant.Capability == guestModel.CapabilityPost, Messages: []GuestMessage{}}
	if len(posts) > postDomain.POST_COUNT {
		posts, view.HasMore = posts[:postDomain.POST_COUNT], true
	}
	for _, p := range posts {
		m := GuestMessage{ID: p.Uuid, Author: authorName(p.PostBy), Text: PlainText(p.Text)}
		if p.CreatedAt != nil {
			m.CreatedAt = *p.CreatedAt
		}
		if p.CommentCount != nil {
			m.ReplyCount = *p.CommentCount
		}
		view.Messages = append(view.Messages, m)
	}
	return view, nil
}

// GuestThread is one message and its replies.
type GuestThread struct {
	Message GuestMessage   `json:"message"`
	Replies []GuestMessage `json:"replies"`
}

// GetGuestThread is a message of the guest's channel with its replies. A
// message from any other channel answers as one that doesn't exist.
func GetGuestThread(ctx context.Context, grant *guestModel.GuestGrant, postID string) (*GuestThread, error) {
	channelID, err := channelOf(grant)
	if err != nil {
		return nil, err
	}
	bot, err := userBusiness.EnsureChannelGuestBot(ctx)
	if err != nil {
		return nil, err
	}
	p, err := postDomain.GetDgraphPostByUUIDWithAllComments(ctx, postID, bot.DgraphUID)
	if err != nil || p == nil || p.Channel == nil || p.Channel.Uuid != channelID.String() || helpers.IsSoftDeleted(p.DeletedAt) {
		return nil, ErrNotFound
	}
	t := &GuestThread{Message: GuestMessage{ID: p.Uuid, Author: authorName(p.PostBy), Text: PlainText(p.Text)}, Replies: []GuestMessage{}}
	if p.CreatedAt != nil {
		t.Message.CreatedAt = *p.CreatedAt
	}
	for _, c := range p.Comments {
		r := GuestMessage{ID: c.Uuid, Author: authorName(c.CommentBy), Text: PlainText(c.Text)}
		if c.CreatedAt != nil {
			r.CreatedAt = *c.CreatedAt
		}
		t.Replies = append(t.Replies, r)
	}
	t.Message.ReplyCount = uint64(len(t.Replies))
	return t, nil
}

// GuestMessageHTML is what a guest wrote, ready to post: escaped (a guest is
// untrusted, and what they write renders inside members' sessions), line
// breaks kept, and led by their name marked as a guest. Pure.
func GuestMessageHTML(name, text string) (string, error) {
	name = SanitizeGuestName(name)
	if name == "" {
		return "", &ErrGuestInput{"Enter your name."}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", &ErrGuestInput{"Write a message first."}
	}
	if utf8.RuneCountInString(text) > maxGuestMessage {
		return "", &ErrGuestInput{"Keep messages under 4,000 characters."}
	}
	body := "<p>" + strings.ReplaceAll(html.EscapeString(text), "\n", "<br>") + "</p>"
	return botpost.LabelledHTML(body, name+" (guest)"), nil
}

// PostAsGuest writes in the guest's channel, or replies in a thread of it.
func PostAsGuest(ctx context.Context, grant *guestModel.GuestGrant, name, text, replyTo string) error {
	channelID, err := channelOf(grant)
	if err != nil {
		return err
	}
	if grant.Capability != guestModel.CapabilityPost {
		return ErrForbidden
	}
	body, err := GuestMessageHTML(name, text)
	if err != nil {
		return err
	}
	bot, err := userBusiness.EnsureChannelGuestBot(ctx)
	if err != nil {
		return err
	}
	if replyTo != "" {
		postID, err := uuid.Parse(replyTo)
		if err != nil {
			return ErrNotFound
		}
		p, err := postDomain.GetDgraphPostByUUIDWithAllComments(ctx, postID.String(), bot.DgraphUID)
		if err != nil || p == nil || p.Channel == nil || p.Channel.Uuid != channelID.String() || helpers.IsSoftDeleted(p.DeletedAt) {
			return ErrNotFound
		}
		_, err = botpost.PostCommentToPostAsBot(ctx, postID, body, bot)
		return err
	}
	_, err = botpost.PostToChannelAsBot(ctx, channelID, body, bot)
	return err
}
