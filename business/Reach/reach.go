// Package business (Reach) is who a message reaches beyond the people it
// names (chat P3; design/chat-vs-slack/PROGRAM.md B5): @channel and
// @everyone (everyone in the channel), @here (those of them who are here now:
// a device connected, not set offline) and a team's mention (its people who
// can read the message: Slack's user groups, free on every plan, O8).
//
// The composer writes them as mention chips whose data-id says what they are
// ("broadcast:channel", "group:<team uuid>"), never a person's "<uuid>@<uid>",
// so helpers.GetMentions passes over them. Sending resolves them into the
// people they reach, who join the message's mentions: each is told as anyone
// mentioned is (their setting for the conversation decides: "Nothing" still
// hears nothing), and the message is in their Mentions. A bot is never
// reached, so a broadcast wakes no agent; nor is the writer.
//
// The rules: a channel's managers and the workspace's admins may notify all
// of it; members may unless the workspace or the channel says not
// (policy chat.broadcast.members), or the channel is larger than
// chat.broadcast.admins_only_at. A broadcast is never sent inside a thread or
// outside a channel: there it reaches nobody (the composer doesn't offer it).
package business

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	presenceBusiness "github.com/akashc777/OneCamp/business/Presence"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	channelLevels "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// MaxReach is the most people one message reaches beyond those it names: a
// channel larger than this is notified up to it, and the rest read it in the
// channel.
const MaxReach = 5000

// Broadcast is a mention of a whole room.
type Broadcast string

const (
	// Channel is everyone in the channel.
	Channel Broadcast = "channel"
	// Here is everyone in the channel who is here now (domain/Message
	// ReachedPerson.Here).
	Here Broadcast = "here"
	// Everyone is everyone in the channel, as Slack's #general reads it.
	Everyone Broadcast = "everyone"
)

// Asked is what a message asks to reach beyond the people it names.
type Asked struct {
	Broadcasts []Broadcast
	// Groups are the teams it mentions, by uuid.
	Groups []string
}

// Empty is whether it asks to reach nobody beyond those it names.
func (a Asked) Empty() bool { return len(a.Broadcasts) == 0 && len(a.Groups) == 0 }

// chip is a mention chip of a room or a team, as the composer writes it.
var chip = regexp.MustCompile(`data-id="(broadcast|group):([^"]+)"`)

// Parse is what html asks to reach beyond the people it names: the broadcasts
// and teams its chips name, each once, in the order written. Pure.
func Parse(html string) Asked {
	var a Asked
	for _, m := range chip.FindAllStringSubmatch(html, -1) {
		switch m[1] {
		case "broadcast":
			b := Broadcast(strings.ToLower(m[2]))
			if (b == Channel || b == Here || b == Everyone) && !slices.Contains(a.Broadcasts, b) {
				a.Broadcasts = append(a.Broadcasts, b)
			}
		case "group":
			if id := strings.ToLower(m[2]); !slices.Contains(a.Groups, id) {
				a.Groups = append(a.Groups, id)
			}
		}
	}
	return a
}

// Seams for tests.
var (
	memberIDs = func(ctx context.Context, channel string) ([]string, error) {
		return channelLevels.MemberIDs(ctx, channel, MaxReach)
	}
	readPeople = userDomain.GetActiveDgraphUsersByUUIDsLight
	readTeams  = teamDomain.TeamsWithMembers
	membersMay = func(ctx context.Context, channel string) bool {
		return policyBusiness.Policies.BoolIn(ctx, policyBusiness.ChatBroadcastMembers, policyBusiness.Scope{Kind: policyBusiness.ScopeChannel, ID: channel})
	}
	adminsOnlyAt = func(ctx context.Context) int {
		return policyBusiness.Policies.Int(ctx, policyBusiness.ChatBroadcastAdminsOnlyAt)
	}
)

// Manages is whether who may always notify a whole channel: one of its
// managers or one of the workspace's admins, in the app. Through a token
// neither (helpers.ActsThroughToken: a token acts as a member, as an
// announcement channel's posting rule has it). ch is the channel as read for
// who.
func Manages(ctx context.Context, who *userModels.UserInfo, ch *dgraphStruct.DgraphChannel) bool {
	if helpers.ActsThroughToken(ctx) {
		return false
	}
	if ch != nil && ch.IsAdmin > 0 {
		return true
	}
	return who != nil && who.UserPostgresInfo.IsAdmin
}

// Refusal is a rule saying no to a broadcast, in the words the person reads.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// Check is whether who may send asked into the channel ch (as read for who):
// nil when they may, else the rule's words. Only broadcasts have rules; a
// team's mention reaches only those of its people who can read the message.
func Check(ctx context.Context, who *userModels.UserInfo, ch *dgraphStruct.DgraphChannel, asked Asked) *Refusal {
	if len(asked.Broadcasts) == 0 || ch == nil || Manages(ctx, who, ch) {
		return nil
	}
	name := "this channel"
	if ch.Name != "" {
		name = "#" + ch.Name
	}
	if !membersMay(ctx, ch.Uuid) {
		return &Refusal{Msg: fmt.Sprintf("Only %s's managers can notify everyone in it.", name)}
	}
	if n := adminsOnlyAt(ctx); n > 0 && int(ch.MemberCount) > n {
		return &Refusal{Msg: fmt.Sprintf("%s has more than %d members, so only its managers can notify all of it.", capitalised(name), n)}
	}
	return nil
}

func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// InChannel is the people asked reaches in the channel, written by who: all
// its members for @channel or @everyone, those here now for @here, and the
// people who are members of a team who may mention (MayMention). Never the
// writer or a bot. Up to MaxReach people.
func InChannel(ctx context.Context, channelID string, asked Asked, who *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
	if asked.Empty() {
		return nil, nil
	}
	members, err := memberIDs(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return reached(ctx, members, asked, who, true)
}

// InThread is InChannel for a reply in one of the channel's threads: a team's
// mention reaches as it does in the channel, and a broadcast reaches nobody.
func InThread(ctx context.Context, channelID string, asked Asked, who *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
	asked.Broadcasts = nil
	return InChannel(ctx, channelID, asked, who)
}

// AmongPeople is the people a team's mention reaches in a DM or a group: its
// people among those in the conversation (people, user uuids). A broadcast
// reaches nobody outside a channel.
func AmongPeople(ctx context.Context, people []string, asked Asked, who *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
	asked.Broadcasts = nil
	if asked.Empty() {
		return nil, nil
	}
	return reached(ctx, people, asked, who, false)
}

// MayMention is whether who may mention the team t: one of its members, or one
// of the workspace's admins in the app. A team's people are its members' and
// admins' to see (business/Team), so a chip of any other team reaches nobody.
func MayMention(ctx context.Context, who *userModels.UserInfo, t *dgraphStruct.DgraphTeam) bool {
	if who == nil || t == nil {
		return false
	}
	if who.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(ctx) {
		return true
	}
	return slices.ContainsFunc(t.Members, func(m *dgraphStruct.DgraphUser) bool {
		return m != nil && (m.Uid != "" && m.Uid == who.UserDgraphInfo.Uid || strings.EqualFold(m.Uuid, who.UserDgraphInfo.Uuid))
	})
}

// reached is who of in (user uuids) asked reaches, written by who: everyone in
// for @channel or @everyone, those here now for @here, and the people among in
// of the teams who may mention.
func reached(ctx context.Context, in []string, asked Asked, who *userModels.UserInfo, channel bool) ([]*dgraphStruct.DgraphUser, error) {
	sender := ""
	if who != nil {
		sender = who.UserDgraphInfo.Uuid
	}
	inSet := make(map[string]bool, len(in))
	for _, id := range in {
		inSet[strings.ToLower(id)] = true
	}
	all, here := false, false
	if channel {
		for _, b := range asked.Broadcasts {
			switch b {
			case Channel, Everyone:
				all = true
			case Here:
				here = true
			}
		}
	}
	// A team's people are reached whether here or not.
	inTeams := map[string]bool{}
	if len(asked.Groups) > 0 {
		teams, err := readTeams(ctx, asked.Groups)
		if err != nil {
			return nil, err
		}
		for _, t := range teams {
			if !MayMention(ctx, who, t) {
				continue
			}
			for _, m := range t.Members {
				if m == nil {
					continue
				}
				if id := strings.ToLower(m.Uuid); inSet[id] {
					inTeams[id] = true
				}
			}
		}
	}
	want := inTeams
	if all || here {
		want = inSet
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		if id != strings.ToLower(sender) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)
	if len(ids) > MaxReach {
		ids = ids[:MaxReach]
	}
	people, err := readPeople(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*dgraphStruct.DgraphUser, 0, len(people))
	for _, p := range people {
		if p == nil || p.Uid == "" {
			continue
		}
		id := strings.ToLower(p.Uuid)
		if p.IsBot || id == strings.ToLower(sender) {
			continue
		}
		// Who only @here reaches must be here now.
		if !all && here && !inTeams[id] && !presenceBusiness.Here(p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Merge is mentions with extra added, each person once by their graph uid, in
// the order given. Pure.
func Merge(mentions, extra []*dgraphStruct.DgraphUser) []*dgraphStruct.DgraphUser {
	if len(extra) == 0 {
		return mentions
	}
	seen := make(map[string]bool, len(mentions)+len(extra))
	out := make([]*dgraphStruct.DgraphUser, 0, len(mentions)+len(extra))
	for _, list := range [][]*dgraphStruct.DgraphUser{mentions, extra} {
		for _, u := range list {
			if u == nil || u.Uid == "" || seen[u.Uid] {
				continue
			}
			seen[u.Uid] = true
			out = append(out, u)
		}
	}
	return out
}
