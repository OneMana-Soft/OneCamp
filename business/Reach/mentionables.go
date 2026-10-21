package business

// What the "@" list needs from the server for one conversation (chat P3,
// D-M9; MC-82 to MC-85, MC-90): who is in it, those who wrote in it lately
// first; the teams to offer, with how many of each are in it; the broadcasts,
// or why they're off here; and the zones people set, for "23:40 for Jonas".
// The list's people are the workspace's, which the app already keeps
// (workspaceMembers); this says which of them are here.

import (
	"context"
	"slices"
	"sort"
	"strings"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	domain "github.com/akashc777/OneCamp/domain/Message"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// Place is where a composer writes.
type Place struct {
	// Kind is "channel", "dm" or "group"; ID the channel's uuid or the
	// conversation's grouping id.
	Kind, ID string
	// Thread is whether it writes a reply in one of the place's threads.
	Thread bool
	// Channel is the channel as read for the writer (channels only).
	Channel *dgraphStruct.DgraphChannel
	// People are a DM's or a group's people, by user uuid (others only).
	People []string
}

// Mentionables is the server's part of the "@" list for one place.
type Mentionables struct {
	// Me is the writer, whom the list offers last.
	Me string `json:"me"`
	// Name is the channel's name, for "Not in #launch" (channels only).
	Name string `json:"name,omitempty"`
	// Recent is who wrote here lately, the most recent first, of those still
	// here (up to RecentListed).
	Recent []string `json:"recent"`
	// Members is who is here, up to MembersListed; Count how many in all.
	// Beyond the list the app can't say who isn't here, so it doesn't.
	Members []string `json:"members"`
	Count   int      `json:"count"`
	// Teams are the workspace's teams, by name.
	Teams []TeamOffer `json:"teams"`
	// Broadcasts are the room's mentions this place has; Why says why they
	// notify no one here when they don't (a rule, or a thread), and the list
	// shows them greyed with it.
	Broadcasts []BroadcastOffer `json:"broadcasts"`
	Why        string           `json:"why,omitempty"`
	// Zones are the zones the people here set, by user uuid.
	Zones map[string]string `json:"zones,omitempty"`
}

// TeamOffer is a team as the "@" list offers it.
type TeamOffer struct {
	UUID string `json:"team_uuid"`
	Name string `json:"name"`
	// Members is how many people are in it, and Here how many of them a
	// mention of it here would reach (never the writer).
	Members int `json:"members"`
	Here    int `json:"here"`
}

// BroadcastOffer is a room's mention as the "@" list offers it, with at most
// how many it notifies: everyone here but the writer (0 for @here, whose
// count is whoever is here when it is sent).
type BroadcastOffer struct {
	Name  Broadcast `json:"name"`
	Count int       `json:"count"`
}

const (
	// MembersListed is how many of a place's people Mentionables lists.
	MembersListed = 1000
	// RecentListed is how many recent writers it lists.
	RecentListed = 20
)

// Seams for tests.
var (
	recentWriters = domain.RecentWriters
	// The teams the "@" list offers: the writer's own, every team for the
	// workspace's admins in the app (MayMention's rule).
	teamsToOffer = func(ctx context.Context, who *userModels.UserInfo) ([]*dgraphStruct.DgraphTeam, error) {
		admin := who.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(ctx)
		return teamDomain.TeamsWithMembersOf(ctx, who.UserDgraphInfo.Uid, admin)
	}
	zonesOf        = prefModels.ZonesOf
	defaultChannel = func(id string) bool {
		ids, _ := settingsBusiness.DefaultChannelIDs()
		return slices.ContainsFunc(ids, func(x string) bool { return strings.EqualFold(x, id) })
	}
)

// threadWhy is why a broadcast notifies no one from a thread (TN-71).
const threadWhy = "Threads don't notify a whole channel."

// MentionablesIn is the "@" list's server part for who writing at place.
func MentionablesIn(ctx context.Context, who *userModels.UserInfo, place Place) (Mentionables, error) {
	me := strings.ToLower(who.UserDgraphInfo.Uuid)
	members := place.People
	if place.Kind == "channel" {
		ids, err := memberIDs(ctx, place.ID)
		if err != nil {
			return Mentionables{}, err
		}
		members = ids
	}
	here := make(map[string]bool, len(members))
	for _, id := range members {
		here[strings.ToLower(id)] = true
	}
	m := Mentionables{Me: me, Count: len(members), Members: listed(members, MembersListed)}

	recent, err := recentWriters(ctx, place.Kind, place.ID, RecentListed*2)
	if err != nil {
		return Mentionables{}, err
	}
	for _, id := range recent {
		if here[strings.ToLower(id)] && len(m.Recent) < RecentListed {
			m.Recent = append(m.Recent, id)
		}
	}

	teams, err := teamsToOffer(ctx, who)
	if err != nil {
		return Mentionables{}, err
	}
	m.Teams = offersOf(teams, here, me)

	if m.Zones, err = zonesOf(ctx, m.Members); err != nil {
		return Mentionables{}, err
	}

	if place.Kind == "channel" && place.Channel != nil {
		m.Name = place.Channel.Name
		m.Broadcasts = []BroadcastOffer{{Name: Here}, {Name: Channel, Count: max(len(members)-1, 0)}}
		if defaultChannel(place.ID) {
			// @everyone where everyone is: a channel each new member joins.
			m.Broadcasts = append(m.Broadcasts, BroadcastOffer{Name: Everyone, Count: max(len(members)-1, 0)})
		}
		switch {
		case place.Thread:
			m.Why = threadWhy
		default:
			if r := Check(ctx, who, place.Channel, Asked{Broadcasts: []Broadcast{Channel}}); r != nil {
				m.Why = r.Msg
			}
		}
	}
	return m, nil
}

// listed is the first n of ids. Pure.
func listed(ids []string, n int) []string {
	if len(ids) > n {
		return slices.Clone(ids[:n])
	}
	return slices.Clone(ids)
}

// offersOf is teams as the list offers them, by name: how many people each
// has, and how many of them are here (never me). Pure.
func offersOf(teams []*dgraphStruct.DgraphTeam, here map[string]bool, me string) []TeamOffer {
	out := make([]TeamOffer, 0, len(teams))
	for _, t := range teams {
		if t == nil {
			continue
		}
		o := TeamOffer{UUID: t.Uuid, Name: t.Name, Members: len(t.Members)}
		for _, m := range t.Members {
			if m == nil {
				continue
			}
			if id := strings.ToLower(m.Uuid); here[id] && id != me {
				o.Here++
			}
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
