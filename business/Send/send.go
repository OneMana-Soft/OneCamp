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
	"net/http"

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

// PrepareChannelPost checks that the person may post this in the channel.
func PrepareChannelPost(ctx context.Context, user *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo) (*ChannelPost, error) {
	channelUUID, err := uuid.Parse(in.ChannelUuid)
	if err != nil {
		return nil, reject(http.StatusBadRequest, "Failed to get channel UUID", err)
	}
	in.ChannelUUID = channelUUID
	channel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, user.UserDgraphInfo.Uid)
	if err != nil || channel == nil || !channel.DeletedAt.IsZero() {
		return nil, reject(http.StatusBadRequest, "Failed to get dgraph channel info", err)
	}
	if channel.IsMember == 0 {
		return nil, reject(http.StatusForbidden, "Not Authorised", nil)
	}
	// Announcement channels: only channel admins post; everyone reads.
	if channel.PostPolicy == "admins_only" && channel.IsAdmin == 0 {
		return nil, reject(http.StatusForbidden, "Only channel admins can post in this announcement channel", nil)
	}
	mentions, err := resolveMentions(ctx, in.HTMLText)
	if err != nil {
		return nil, err
	}
	return &ChannelPost{in: in, user: user, channel: channel, mentions: mentions}, nil
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
	if err != nil || to == nil || (to.DeletedAt != nil && !to.DeletedAt.IsZero()) {
		return nil, reject(http.StatusBadRequest, "Failed to info of  toUUID in req", err)
	}
	// One rule for who can be DMed, shared with the AI executor and MCP.
	if e := principalBusiness.CanReceiveDirectMessage(to); !e.Allowed {
		return nil, reject(http.StatusBadRequest, "Cannot start a direct message with this user", errors.New(e.Reason))
	}
	mentions, err := resolveMentions(ctx, in.TextHtml)
	if err != nil {
		return nil, err
	}
	return &DirectMessage{in: in, user: user, to: to, toUUID: toUUID, mentions: mentions}, nil
}

// Recipient is who the message goes to, for naming them to the person.
func (m *DirectMessage) Recipient() *dgraphStruct.DgraphUser { return m.to }

// Commit writes the direct message.
func (m *DirectMessage) Commit(ctx context.Context) (*chatAdapter.OutputCreateChatForChat, error) {
	return chatBusiness.CreateChat(ctx, m.in, m.user, m.to, m.toUUID, m.mentions)
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
func PrepareGroupMessage(ctx context.Context, user *userModels.UserInfo, in *chatAdapter.ChatInfo) (*GroupMessage, error) {
	var participants []*dgraphStruct.DgraphUser
	if len(in.Participants) > 0 {
		users, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, in.Participants)
		if len(users) != len(in.Participants) || err != nil {
			return nil, reject(http.StatusBadRequest, "Got unregistered users in participants", err)
		}
		for _, p := range users {
			if p.IsExternal {
				return nil, reject(http.StatusBadRequest, "Cannot add external users to group chats", nil)
			}
		}
		participants = users
	} else if len(in.GrpUuid) > 0 {
		dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, user.UserDgraphInfo.Uid, in.GrpUuid)
		if err != nil || dm == nil {
			return nil, reject(http.StatusBadRequest, "Faild to get group participants", err)
		}
		if dm.ParticipantIsMember == 0 {
			return nil, reject(http.StatusForbidden, "Not Authorised", nil)
		}
		participants = dm.Participants
	}
	if len(participants) < 3 {
		return nil, reject(http.StatusBadRequest, "Minimum 3 participants required for a group", nil)
	}
	mentions, err := resolveMentions(ctx, in.TextHtml)
	if err != nil {
		return nil, err
	}
	return &GroupMessage{in: in, user: user, participants: participants, mentions: mentions}, nil
}

// Participants are the people in the group, for naming it to the person.
func (m *GroupMessage) Participants() []*dgraphStruct.DgraphUser { return m.participants }

// Commit writes the group message.
func (m *GroupMessage) Commit(ctx context.Context) (*chatAdapter.OutputCreateChatForChat, error) {
	return chatBusiness.CreateChatForGroup(ctx, m.in, m.user, m.mentions, m.participants)
}
