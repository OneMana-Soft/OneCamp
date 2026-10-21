// Package business (Send) is "send a message as this person": a channel post,
// a direct message, or a group message, with every rule the HTTP handlers
// apply. Each kind is split into Prepare (who may send what, where, and who is
// mentioned) and Commit (the write), so a scheduled message can be checked
// when it is scheduled and checked again when it fires, by the same code that
// checks a message sent now. Before this, the rules lived only in the
// controllers, and a second sender would have had to copy them.
package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChannel"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Rejection is a rule saying no, with the status and message the HTTP handler
// has always answered with, so moving the rule here changed no response.
type Rejection struct {
	Status int
	Msg    string
	Err    error
}

func (r *Rejection) Error() string {
	if r.Err != nil {
		return r.Msg + ": " + r.Err.Error()
	}
	return r.Msg
}

// AsRejection reports whether err is a Rejection (a rule, not a fault).
func AsRejection(err error) (*Rejection, bool) {
	var r *Rejection
	ok := errors.As(err, &r)
	return r, ok
}

func reject(status int, msg string, err error) error {
	return &Rejection{Status: status, Msg: msg, Err: err}
}

// resolveMentions turns the @-mentions in a message into users, refusing any
// that are not registered (the composer only offers real people).
func resolveMentions(ctx context.Context, html string) ([]*dgraphStruct.DgraphUser, error) {
	mentions, err := helpers.GetMentions(html)
	if err != nil {
		return nil, reject(http.StatusBadRequest, "Failed to get post mentions", err)
	}
	users, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)
	if len(users) != len(mentions) {
		return nil, reject(http.StatusBadRequest, "Got unregistered users in mentions", err)
	}
	return users, nil
}

// ---------------------------------------------------------------- channel post

// ChannelPost is a channel post that has passed every rule and can be written.
type ChannelPost struct {
	in       *postAdapter.InputCreateOrUpdatePostInfo
	user     *userModels.UserInfo
	channel  *dgraphStruct.DgraphChannel
	mentions []*dgraphStruct.DgraphUser
}

// PrepareChannelPost checks that the person may post this in the channel, and
// who it tells as mentioned: the people it names and those its broadcasts and
// team mentions reach (Mentions).
func PrepareChannelPost(ctx context.Context, user *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo) (*ChannelPost, error) {
	return prepareChannelPost(ctx, user, in, true)
}

// PrepareReplyCopy is PrepareChannelPost for the copy of a thread reply sent
// to the channel too ("Also send to #channel"): the same rules for posting,
// and nobody told as mentioned. The reply tells the people it mentions; the
// copy told them a second time and listed the mention twice in their
// Activity. A broadcast reaches nobody from a thread (TN-71), so the copy
// carries one in its words without notifying anyone, and is never refused
// for it. Its text still shows them.
func PrepareReplyCopy(ctx context.Context, user *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo) (*ChannelPost, error) {
	return prepareChannelPost(ctx, user, in, false)
}

func prepareChannelPost(ctx context.Context, user *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo, mentioning bool) (*ChannelPost, error) {
	channelUUID, err := uuid.Parse(in.ChannelUuid)
	if err != nil {
		return nil, reject(http.StatusBadRequest, "Failed to get channel UUID", err)
	}
	in.ChannelUUID = channelUUID
	channel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, user.UserDgraphInfo.Uid)
	if err != nil && !errors.Is(err, channelBusiness.ErrNotFound) {
		// The graph didn't answer: a fault, not a rule, so a message sent
		// later (a scheduled one, a reminder) is tried again, not dropped.
		return nil, fmt.Errorf("reading the channel: %w", err)
	}
	if rj := MayPostIn(channel); rj != nil {
		return nil, rj
	}
	// Through an API token (the public API, MCP, an agent run one asked for), never
	// as one of the channel's admins: a token posts as a member, and an announcement
	// channel's admins post in it in the app, as themselves (helpers.ActsThroughToken).
	if channel.PostPolicy == "admins_only" && helpers.ActsThroughToken(ctx) {
		return nil, &Rejection{Status: http.StatusForbidden, Msg: "Only channel admins can post in this announcement channel, in the app: a token posts as a member"}
	}
	if !mentioning {
		return &ChannelPost{in: in, user: user, channel: channel}, nil
	}
	mentions, err := Mentions(ctx, user, Where{Channel: channel}, &in.HTMLText)
	if err != nil {
		return nil, err
	}
	return &ChannelPost{in: in, user: user, channel: channel, mentions: mentions}, nil
}

// MayPostIn is the rule for writing in a channel, given the channel as read
// for the person (its member and admin counts are theirs): it is live, they
// are in it, and in an announcement channel they are one of its admins.
// PrepareChannelPost asks it, and so does anything that writes into channels
// its own way (a forward), so the rule has one copy.
func MayPostIn(channel *dgraphStruct.DgraphChannel) *Rejection {
	if channel == nil || helpers.IsSoftDeleted(channel.DeletedAt) {
		return &Rejection{Status: http.StatusBadRequest, Msg: "Failed to get dgraph channel info"}
	}
	if channel.IsMember == 0 {
		return &Rejection{Status: http.StatusForbidden, Msg: "Not Authorised"}
	}
	// Announcement channels: only channel admins post; everyone reads.
	if channel.PostPolicy == "admins_only" && channel.IsAdmin == 0 {
		return &Rejection{Status: http.StatusForbidden, Msg: "Only channel admins can post in this announcement channel"}
	}
	return nil
}

// Channel is the channel the post goes to, for naming it to the person.
func (p *ChannelPost) Channel() *dgraphStruct.DgraphChannel { return p.channel }

// Commit writes the post and marks the channel read up to it for the sender.
func (p *ChannelPost) Commit(ctx context.Context) (*postAdapter.OutputCreatePostForPost, error) {
	created, err := postBusiness.CreatePost(ctx, p.in, p.user, p.mentions, p.channel)
	if err != nil {
		return nil, err
	}
	if lerr := lastseenBusiness.CreateOrUpdateLastSeenChannel(ctx, p.user.UserPostgresInfo.Id, p.in.ChannelUUID); lerr != nil {
		helpers.LogErrorWithContext(ctx, "Send/ChannelPost.Commit last seen err: %+v", lerr)
	}
	return created, nil
}

// CommitDurably writes the post as the durable pipeline does (chat A2,
// business/Post durable.go): its rows and events at once, its side effects
// from the outbox; clientMsgID is the id the app gave it. Then, as Commit, the
// channel is read up to it for the sender.
func (p *ChannelPost) CommitDurably(ctx context.Context, clientMsgID string) (*postAdapter.OutputCreatePostForPost, error) {
	created, err := postBusiness.CreatePostDurably(ctx, p.in, p.user, p.mentions, p.channel, clientMsgID)
	if err != nil {
		return nil, durableRefusal(err)
	}
	if lerr := lastseenBusiness.CreateOrUpdateLastSeenChannel(ctx, p.user.UserPostgresInfo.Id, p.in.ChannelUUID); lerr != nil {
		helpers.LogErrorWithContext(ctx, "Send/ChannelPost.CommitDurably last seen err: %+v", lerr)
	}
	return created, nil
}

// durableRefusal is a durable write's error as the routes answer it: a retry
// whose first try hasn't finished is answered as Once answers a send in
// flight, so the app tries again. Pure.
func durableRefusal(err error) error {
	if errors.Is(err, postBusiness.ErrStillBeingSent) || errors.Is(err, chatBusiness.ErrStillBeingSent) {
		return reject(http.StatusConflict, "This message is still being sent", err)
	}
	return err
}

// AlsoInChannel posts html in a channel as the person, for a feature that
// offers "also post it in a channel" beside what it saves (a project update,
// a goal check-in). It answers the channel's name, or, when the channel
// refused it or the post failed, a sentence saying so that begins with saved
// ("Posted on the project"): what was saved stays saved either way.
func AlsoInChannel(ctx context.Context, user *userModels.UserInfo, channelUUID, html, saved string) (channel, problem string) {
	post, err := PrepareChannelPost(ctx, user, &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channelUUID, HTMLText: html})
	if err == nil {
		_, err = post.Commit(ctx)
	}
	if err == nil {
		return post.Channel().Name, ""
	}
	if r, ok := AsRejection(err); ok && r.Status < http.StatusInternalServerError {
		return "", saved + ", but not in the channel: " + r.Msg + "."
	}
	helpers.LogErrorWithContext(ctx, "Send/AlsoInChannel err: %+v", err)
	return "", saved + ", but not in the channel."
}

// --------------------------------------------------------------- direct message

// DirectMessage is a DM that has passed every rule and can be written.
type DirectMessage struct {
	in       *chatAdapter.ChatInfo
	user     *userModels.UserInfo
	to       *dgraphStruct.DgraphUser
	toUUID   uuid.UUID
	mentions []*dgraphStruct.DgraphUser
}

// PrepareDirectMessage checks that the person may message the recipient.
func PrepareDirectMessage(ctx context.Context, user *userModels.UserInfo, in *chatAdapter.ChatInfo) (*DirectMessage, error) {
	toUUID, err := uuid.Parse(in.ToUuid)
	if err != nil {
		return nil, reject(http.StatusBadRequest, "Failed to parse toUUID in req", err)
	}
	to, err := userBusiness.GetDgraphUserInfoByUUID(ctx, in.ToUuid)
	if err != nil || to == nil {
		return nil, reject(http.StatusBadRequest, "Failed to info of  toUUID in req", err)
	}
	if rj := MayMessage(to); rj != nil {
		return nil, rj
	}
	mentions, err := Mentions(ctx, user, Where{People: []string{user.UserDgraphInfo.Uuid, to.Uuid}}, &in.TextHtml)
	if err != nil {
		return nil, err
	}
	return &DirectMessage{in: in, user: user, to: to, toUUID: toUUID, mentions: mentions}, nil
}

// MayMessage is the rule for writing to a person directly: an account that
// can receive direct messages (principalBusiness.CanReceiveDirectMessage, the
// rule the AI executor and MCP share, which refuses a deactivated one) and
// carries no deletion time. PrepareDirectMessage asks it, and so does anything
// that writes to a person its own way (a forward).
func MayMessage(to *dgraphStruct.DgraphUser) *Rejection {
	if e := principalBusiness.CanReceiveDirectMessage(to); !e.Allowed {
		return &Rejection{Status: http.StatusBadRequest, Msg: "Cannot start a direct message with this user", Err: errors.New(e.Reason)}
	}
	if to.DeletedAt != nil && !to.DeletedAt.IsZero() {
		return &Rejection{Status: http.StatusBadRequest, Msg: "Failed to info of  toUUID in req"}
	}
	return nil
}

// Commit writes the direct message.
func (m *DirectMessage) Commit(ctx context.Context) (*chatAdapter.OutputCreateChatForChat, error) {
	return chatBusiness.CreateChat(ctx, m.in, m.user, m.to, m.toUUID, m.mentions)
}

// CommitDurably writes the direct message as the durable pipeline does (chat
// A2, business/Chat durable.go): its rows and events at once, its side
// effects from the outbox; clientMsgID is the id the app gave it.
func (m *DirectMessage) CommitDurably(ctx context.Context, clientMsgID string) (*chatAdapter.OutputCreateChatForChat, error) {
	created, err := chatBusiness.CreateDMDurably(ctx, m.in, m.user, m.to, m.toUUID, m.mentions, clientMsgID)
	if err != nil {
		return nil, durableRefusal(err)
	}
	return created, nil
}

// ---------------------------------------------------------------- group message

// GroupMessage is a group message that has passed every rule and can be written.
type GroupMessage struct {
	in           *chatAdapter.ChatInfo
	user         *userModels.UserInfo
	participants []*dgraphStruct.DgraphUser
	mentions     []*dgraphStruct.DgraphUser
}

// PrepareGroupMessage checks the group (new or existing) and the sender's place in it.
//
// An existing group's people are the group's, whatever the request lists, and
// the sender must be one of them. A new group must include the sender: its id
// is made from its people, so a list without the sender wrote into the group
// those people already had, which the sender wasn't in. Both were open.
func PrepareGroupMessage(ctx context.Context, user *userModels.UserInfo, in *chatAdapter.ChatInfo) (*GroupMessage, error) {
	var participants []*dgraphStruct.DgraphUser
	if len(in.GrpUuid) > 0 {
		dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, user.UserDgraphInfo.Uid, in.GrpUuid)
		if err != nil || dm == nil {
			return nil, reject(http.StatusBadRequest, "Faild to get group participants", err)
		}
		if dm.ParticipantIsMember == 0 {
			return nil, reject(http.StatusForbidden, "Not Authorised", nil)
		}
		participants = dm.Participants
	} else if len(in.Participants) > 0 {
		users, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, in.Participants)
		if len(users) != len(in.Participants) || err != nil {
			return nil, reject(http.StatusBadRequest, "Got unregistered users in participants", err)
		}
		sender := false
		for _, p := range users {
			if rj := MayBeInGroup(p); rj != nil {
				return nil, rj
			}
			sender = sender || p.Uuid == user.UserDgraphInfo.Uuid
		}
		if !sender {
			return nil, reject(http.StatusForbidden, "A group you start has to include you.", nil)
		}
		participants = users
	}
	if len(participants) < 3 {
		return nil, reject(http.StatusBadRequest, "Minimum 3 participants required for a group", nil)
	}
	mentions, err := Mentions(ctx, user, Where{People: uuidsOf(participants)}, &in.TextHtml)
	if err != nil {
		return nil, err
	}
	return &GroupMessage{in: in, user: user, participants: participants, mentions: mentions}, nil
}

// MayBeInGroup is the rule for being one of a group's people: an account
// someone signs in to (an external identity, imported or synced, never does)
// that isn't deactivated. A new group's people are held to it, and so is
// everyone added to one (business/Conversation AddPeople), so the rule has one
// copy. The external check had read a field the participants' lookup never
// selected, so it never refused anyone.
func MayBeInGroup(u *dgraphStruct.DgraphUser) *Rejection {
	switch {
	case u == nil || u.Uid == "" || u.Uuid == "":
		return &Rejection{Status: http.StatusBadRequest, Msg: "Got unregistered users in participants"}
	case u.IsExternal:
		return &Rejection{Status: http.StatusBadRequest, Msg: "Cannot add external users to group chats"}
	case helpers.IsSoftDeleted(u.DeletedAt):
		return &Rejection{Status: http.StatusBadRequest, Msg: strings.TrimSpace(u.DisplayName() + " can't be added: their account is deactivated.")}
	}
	return nil
}

// Commit writes the group message.
func (m *GroupMessage) Commit(ctx context.Context) (*chatAdapter.OutputCreateChatForChat, error) {
	return chatBusiness.CreateChatForGroup(ctx, m.in, m.user, m.mentions, m.participants)
}

// CommitDurably writes the group message as the durable pipeline does, as a
// direct message's CommitDurably does.
func (m *GroupMessage) CommitDurably(ctx context.Context, clientMsgID string) (*chatAdapter.OutputCreateChatForChat, error) {
	created, err := chatBusiness.CreateGroupMessageDurably(ctx, m.in, m.user, m.mentions, m.participants, clientMsgID)
	if err != nil {
		return nil, durableRefusal(err)
	}
	return created, nil
}
