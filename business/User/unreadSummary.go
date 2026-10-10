package business

import (
	"context"
	"sort"
	"strings"

	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// What a person has not read yet, for clients outside the web app: the
// OneMana kit for Omarchy shows it on the desktop's top bar (GET /v1/unread).
// The counts are the sidebar's own (GetDgraphUserInfoByUUIDForSidebarNav), so
// the bar and the app always agree.

// UnreadPlace is one channel or conversation with unread messages.
type UnreadPlace struct {
	Kind  string `json:"kind"` // channel | dm | group
	Name  string `json:"name"`
	Count uint64 `json:"count"`
	// Path is the web app path (e.g. /app/channel/<id>); the client joins it
	// to the workspace address it already has.
	Path string `json:"path"`
}

// UnreadSummary totals what is unread and lists the busiest places first.
type UnreadSummary struct {
	Total    uint64        `json:"total"`
	Channels uint64        `json:"channels"`
	DMs      uint64        `json:"dms"`
	Activity uint64        `json:"activity"`
	Top      []UnreadPlace `json:"top"`
}

// unreadTopLimit is how many places the summary names; the rest are counted.
const unreadTopLimit = 8

// GetUnreadSummary reads the sidebar's counts for userUUID.
func GetUnreadSummary(ctx context.Context, userUUID uuid.UUID) (*UnreadSummary, error) {
	user, err := GetDgraphUserInfoByUUIDForSidebarNav(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	var activity uint64
	if user != nil {
		activity, _ = lastSeenActivityBusiness.GetTotalUnreadActivityCount(ctx, user.Uid, userUUID)
	}
	return buildUnreadSummary(user, userUUID.String(), activity), nil
}

// buildUnreadSummary turns the sidebar's user into the summary. Pure.
func buildUnreadSummary(user *dgraphStruct.DgraphUser, selfUUID string, activity uint64) *UnreadSummary {
	s := &UnreadSummary{Activity: activity, Top: []UnreadPlace{}}
	if user != nil {
		for _, c := range user.Channels {
			if c == nil || c.UnreadPostCount == 0 {
				continue
			}
			s.Channels += c.UnreadPostCount
			s.Top = append(s.Top, UnreadPlace{Kind: "channel", Name: "#" + c.Name, Count: c.UnreadPostCount, Path: "/app/channel/" + c.Uuid})
		}
		for _, d := range user.DMs {
			if d == nil || d.UnreadMessageCount == 0 {
				continue
			}
			s.DMs += d.UnreadMessageCount
			s.Top = append(s.Top, dmPlace(d, selfUUID))
		}
	}
	s.Total = s.Channels + s.DMs + s.Activity
	sort.SliceStable(s.Top, func(i, j int) bool { return s.Top[i].Count > s.Top[j].Count })
	if len(s.Top) > unreadTopLimit {
		s.Top = s.Top[:unreadTopLimit]
	}
	return s
}

// dmPlace names a conversation by the other people in it and links it the way
// the sidebar does: a one-to-one DM by the other person's id, a group by its
// grouping id.
func dmPlace(d *dgraphStruct.DgraphDm, selfUUID string) UnreadPlace {
	var names, others []string
	for _, p := range d.Participants {
		if p == nil || p.Uuid == selfUUID {
			continue
		}
		others = append(others, p.Uuid)
		if n := p.DisplayName(); n != "" {
			names = append(names, n)
		}
	}
	name := strings.Join(names, ", ")
	if name == "" {
		name = "Direct message"
	}
	if len(others) > 1 {
		return UnreadPlace{Kind: "group", Name: name, Count: d.UnreadMessageCount, Path: "/app/chat/group/" + d.GroupingId}
	}
	path := "/app/chat/"
	if len(others) == 1 {
		path += others[0]
	}
	return UnreadPlace{Kind: "dm", Name: name, Count: d.UnreadMessageCount, Path: path}
}
