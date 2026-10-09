package business

// Where a new member starts.
//
// WHY IT EXISTS. Someone who accepted an invitation landed on an empty Home in
// no channel at all, and the seeded #general post they never saw told them
// "every new member lands here". Joining #general was four more actions of
// their own, and the ones who didn't know to do it never saw a message.
//
// So becoming a member (userBusiness.JoinAsMember, on every way in: an
// invitation, Google or GitHub, OIDC, SAML, LDAP, SCIM) puts the person in the
// workspace's default channels through the ordinary membership path, which
// sets up their last-seen and notification rows and tells user.joined
// listeners. An admin chooses the channels; until one does, it is #general.
//
// NEVER FAILS A JOIN. A channel that is gone, archived or private now, a graph
// that cannot be reached, a write that keeps losing to another join: each is
// logged and skipped. A member left out of #general is a lesser problem than a
// person who cannot get into the workspace at all.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	channelModels "github.com/akashc777/OneCamp/models/postgres/Channel"
	"github.com/dgraph-io/dgo/v230"
	"github.com/google/uuid"
)

// GeneralChannelName is the channel every new workspace starts with
// (business/Onboarding seeds it), and where new members go until an admin
// chooses otherwise.
const GeneralChannelName = "general"

// maxDefaultChannels bounds the choice. Every one is a write per new member,
// and a list longer than this is a workspace that wants a directory, not a
// welcome.
const maxDefaultChannels = 20

// joinableChannelsLimit bounds the list an admin chooses from.
const joinableChannelsLimit = 500

// What SetDefaultChannels refuses, in words for the admin who chose.
var (
	ErrTooManyDefaultChannels = fmt.Errorf("Choose at most %d channels for new members.", maxDefaultChannels)
	ErrNotAChannel            = errors.New("That isn't a channel. Choose from the list.")
	ErrNotJoinable            = errors.New("New members can only join public channels that aren't archived. Choose from the list.")
)

func init() {
	userBusiness.WelcomeNewMembersWith(JoinDefaultChannels)
}

// Channel is a channel by id and name, as the admin chooses it.
type Channel struct {
	UUID uuid.UUID `json:"ch_uuid"`
	Name string    `json:"ch_name"`
}

// DefaultChannels are the channels a new member joins now: the admin's
// choice, or #general when there is none, keeping only those still live and
// public. chosen reports whether an admin made the choice.
func DefaultChannels(ctx context.Context) (channels []Channel, chosen bool, err error) {
	ids, chosen := settingsBusiness.DefaultChannelIDs()
	if !chosen {
		// The #general the workspace was seeded with, by id, so a rename
		// keeps it; else, for a workspace seeded before the pin, by name.
		if pinned := validIDs([]string{settingsBusiness.GeneralChannelID()}); len(pinned) == 1 {
			live, err := domain.PublicLiveChannels(ctx, pinned, 1)
			if err != nil {
				return nil, false, err
			}
			if len(live) == 1 {
				return []Channel{{UUID: live[0].Id, Name: live[0].Name}}, false, nil
			}
		}
		general, err := domain.GetChannelByName(ctx, GeneralChannelName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		if general == nil || general.IsPrivate {
			// No #general (archived, renamed, or a workspace older than
			// seeding), or one made private: nowhere to put anyone.
			return []Channel{}, false, nil
		}
		return []Channel{{UUID: general.Id, Name: general.Name}}, false, nil
	}
	ids = validIDs(ids)
	if len(ids) == 0 {
		return []Channel{}, true, nil
	}
	live, err := domain.PublicLiveChannels(ctx, ids, maxDefaultChannels)
	if err != nil {
		return nil, true, err
	}
	return inOrder(ids, live), true, nil
}

// JoinableChannels lists every live public channel, the ones an admin may
// choose from.
func JoinableChannels(ctx context.Context) ([]Channel, error) {
	live, err := domain.PublicLiveChannels(ctx, nil, joinableChannelsLimit)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(live))
	for _, ch := range live {
		out = append(out, Channel{UUID: ch.Id, Name: ch.Name})
	}
	return out, nil
}

// SetDefaultChannels saves the channels new members join, after checking each
// one is a live public channel: a private one would be a way to hand every
// newcomer a channel its members never opened to them. An empty list is a
// choice too, meaning none.
func SetDefaultChannels(ctx context.Context, ids []string) ([]Channel, error) {
	wanted := validIDs(ids)
	if len(wanted) != len(dedupe(ids)) {
		return nil, ErrNotAChannel
	}
	if len(wanted) > maxDefaultChannels {
		return nil, ErrTooManyDefaultChannels
	}
	channels := []Channel{}
	if len(wanted) > 0 {
		live, err := domain.PublicLiveChannels(ctx, wanted, maxDefaultChannels)
		if err != nil {
			return nil, err
		}
		channels = inOrder(wanted, live)
		if len(channels) != len(wanted) {
			return nil, ErrNotJoinable
		}
	}
	if err := settingsBusiness.SetDefaultChannelIDs(wanted); err != nil {
		return nil, err
	}
	return channels, nil
}

// JoinDefaultChannels puts a new member in the default channels and returns
// the one they land in: #general when they joined it, otherwise the first.
// uuid.Nil means none, and they start on Home. Registered with userBusiness
// as what joining does (init above). It never fails: see the file comment.
func JoinDefaultChannels(ctx context.Context, userID uuid.UUID) (landing uuid.UUID) {
	channels, _, err := DefaultChannels(ctx)
	if err != nil {
		helpers.LogWarnWithContext(ctx, "business/JoinDefaultChannels cannot read the default channels for %s: %+v", userID, err)
		return uuid.Nil
	}
	if len(channels) == 0 {
		return uuid.Nil
	}
	user, err := userDomain.GetDgraphUserInfoByUUID(ctx, userID.String())
	if err != nil || user == nil || user.Uid == "" {
		helpers.LogWarnWithContext(ctx, "business/JoinDefaultChannels no graph node for %s, so no default channels: %+v", userID, err)
		return uuid.Nil
	}
	var joined []Channel
	for _, ch := range channels {
		if err := joinAsNewMember(ctx, ch.UUID, user, userID); err != nil {
			helpers.LogWarnWithContext(ctx, "business/JoinDefaultChannels %s not added to #%s: %+v", userID, ch.Name, err)
			continue
		}
		joined = append(joined, ch)
	}
	return landingOf(joined, settingsBusiness.GeneralChannelID())
}

// SuggestedChannel is the channel Home offers a member who is in none: the
// one a new member lands in (#general when it is a default channel, otherwise
// the first). ok is false when there is none to offer: no default channel, or
// none still public and live.
func SuggestedChannel(ctx context.Context) (Channel, bool, error) {
	channels, _, err := DefaultChannels(ctx)
	if err != nil || len(channels) == 0 {
		return Channel{}, false, err
	}
	landing := landingOf(channels, settingsBusiness.GeneralChannelID())
	for _, ch := range channels {
		if ch.UUID == landing {
			return ch, true, nil
		}
	}
	return Channel{}, false, nil
}

// FirstSignInLanding is where a member whose account the directory made
// (SCIM) starts on their first sign-in, which is when they arrive. They are
// put in the default channels they aren't in yet: one staged before their
// start date (SCIM active:false) and activated some other way than by the
// directory was never welcomed, and joining a channel they are in already is
// nothing. It returns the channel a new member lands in, and uuid.Nil for
// everyone else, who keeps Home.
func FirstSignInLanding(ctx context.Context, userID uuid.UUID) uuid.UUID {
	if !userDomain.FirstSignInOfProvisioned(ctx, userID) {
		return uuid.Nil
	}
	return JoinDefaultChannels(ctx, userID)
}

// joinAsNewMember adds userID to one channel the way joining it does,
// unless they are in it already. It asks the graph rather than trusting the
// setting, because a channel can be archived or made private between an
// admin choosing it and somebody joining.
func joinAsNewMember(ctx context.Context, channelUUID uuid.UUID, user *dgraphStruct.DgraphUser, userID uuid.UUID) error {
	info, err := GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, user.Uid)
	if err != nil {
		return err
	}
	if info != nil && info.IsMember > 0 {
		return nil
	}
	if refusal := CanJoin(info); refusal != nil {
		return refusal
	}
	return addMemberRetrying(ctx, channelUUID, user, userID)
}

// joinAttempts is how often adding a member is tried when the graph aborts it.
//
// Every join to a channel writes the same channel node, so two people joining
// at once (a directory provisioning a team, two invitations accepted in the
// same second) conflict, and Dgraph aborts one and asks for a retry. The
// write is retried a few times already (dgraphInit.DoCommitNow); a burst of
// joins needs more rounds than that, spread out so they stop colliding.
const joinAttempts = 6

func addMemberRetrying(ctx context.Context, channelUUID uuid.UUID, user *dgraphStruct.DgraphUser, userID uuid.UUID) error {
	var err error
	for attempt := 0; attempt < joinAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt*attempt*40+rand.Intn(80)) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = AddChannelMemberEdge(ctx, channelUUID, user, userID); err == nil || !errors.Is(err, dgo.ErrAborted) {
			return err
		}
	}
	return err
}

// landingOf is where someone who joined these channels starts: #general when
// it is among them, it being where the workspace says hello, otherwise the
// first. Pure.
func landingOf(joined []Channel, generalID string) uuid.UUID {
	// The workspace's own #general (pinned by id when it was seeded), then
	// one called general, then the first.
	for _, ch := range joined {
		if generalID != "" && strings.EqualFold(ch.UUID.String(), generalID) {
			return ch.UUID
		}
	}
	for _, ch := range joined {
		if ch.Name == GeneralChannelName {
			return ch.UUID
		}
	}
	if len(joined) > 0 {
		return joined[0].UUID
	}
	return uuid.Nil
}

// validIDs keeps the ids that are uuids, in order, without repeats. Pure.
func validIDs(ids []string) []string {
	out := []string{}
	for _, id := range dedupe(ids) {
		if _, err := uuid.Parse(id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// dedupe keeps the first of each id, compared without case. Pure.
func dedupe(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range ids {
		id := normalizeID(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func normalizeID(raw string) string {
	raw = strings.TrimSpace(raw)
	id, err := uuid.Parse(raw)
	if err != nil {
		return raw
	}
	return id.String()
}

// inOrder is the channels named by ids, in the order of ids. Pure.
func inOrder(ids []string, found []channelModels.Channel) []Channel {
	byID := map[string]Channel{}
	for _, ch := range found {
		byID[ch.Id.String()] = Channel{UUID: ch.Id, Name: ch.Name}
	}
	out := []Channel{}
	for _, id := range ids {
		if ch, ok := byID[id]; ok {
			out = append(out, ch)
		}
	}
	return out
}
