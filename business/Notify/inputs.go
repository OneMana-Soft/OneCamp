package business

import (
	"context"
	"slices"
	"strings"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	chatLevels "github.com/akashc777/OneCamp/business/UserChatNotification"
	channelLevels "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	alertModels "github.com/akashc777/OneCamp/models/postgres/PersonalAlert"
	"github.com/google/uuid"
)

// Inputs are what Plan reads: the sites' own helpers, each labelled with the
// cause it is the audience of, and each a batch, one read for the whole
// event, never one a person. Maps are keyed by lower-case user id.
type Inputs interface {
	// Members is who in a place hears everything there now (its people on
	// "all", every one of them: all_posts', a DM's and a call's audience).
	Members(ctx context.Context, p Place) ([]string, error)
	// Levels is each of ids' setting for the place: "all", "mention" or
	// "block" ("Nothing", muted). Someone with none isn't in the place now:
	// it is the access step's read as much as the preference step's.
	Levels(ctx context.Context, p Place, ids []string) (map[string]string, error)
	// Follows is who turned the thread's following on and off.
	Follows(ctx context.Context, p Place) (notificationBusiness.Follows, error)
	// Reached is who the event's mention of teams (team), or its broadcast
	// (broadcast), reaches in its place now (Plan bounds it by the event's
	// Within).
	Reached(ctx context.Context, ev Event, teams []string, broadcast string) ([]string, error)
	// Keywords is who of a channel's people have a keyword text holds, other
	// than writer.
	Keywords(ctx context.Context, p Place, text, writer string) ([]string, error)
	// VIPs is who of ids has sender as a VIP.
	VIPs(ctx context.Context, sender string, ids []string) (map[string]bool, error)
	// Held is who of ids is held at now (paused, outside their schedule, in
	// focus time), and until when; anyone not held is left out.
	Held(ctx context.Context, ids []string, now time.Time) (map[string]notificationBusiness.HeldStatus, error)
}

// keywordReach is business/Post's keyword reach (keywordReach.go), which Post
// gives the router at init: the router can't import the sites that call it.
var keywordReach func(ctx context.Context, channelID, text, writer string) []string

// ReachKeywordsWith gives the router a channel's keyword reach.
func ReachKeywordsWith(reach func(ctx context.Context, channelID, text, writer string) []string) {
	keywordReach = reach
}

// liveInputs are the inputs as the sites read them today.
type liveInputs struct{}

func (liveInputs) Members(ctx context.Context, p Place) ([]string, error) {
	switch p.Kind {
	case PlaceChannel:
		return channelLevels.MembersOnAll(ctx, p.ID)
	case PlaceDM, PlaceGroup:
		return chatLevels.MembersOnAll(ctx, p.ID)
	}
	return nil, nil
}

func (liveInputs) Levels(ctx context.Context, p Place, ids []string) (map[string]string, error) {
	// Only well-formed ids are asked about: one malformed id would fail the
	// whole read and tell nobody. Anyone else isn't in the place.
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { _, err := uuid.Parse(id); return err != nil })
	switch p.Kind {
	case PlaceChannel:
		return channelLevels.GetLevelsInChannel(ctx, p.ID, ids)
	case PlaceDM, PlaceGroup:
		return chatLevels.GetLevelsInConversation(ctx, p.ID, ids)
	}
	return map[string]string{}, nil
}

func (liveInputs) Follows(ctx context.Context, p Place) (notificationBusiness.Follows, error) {
	on, off, err := threadIndex.Following(ctx, p.ThreadRootID)
	return notificationBusiness.Follows{On: on, Off: off}, err
}

func (liveInputs) Reached(ctx context.Context, ev Event, teams []string, broadcast string) ([]string, error) {
	if ev.Place.Kind != PlaceChannel {
		return nil, nil
	}
	asked := reachBusiness.Asked{Groups: teams}
	if broadcast != "" {
		asked.Broadcasts = []reachBusiness.Broadcast{reachBusiness.Broadcast(broadcast)}
	}
	reach := reachBusiness.InChannel
	if ev.Place.ThreadRootID != "" {
		reach = reachBusiness.InThread
	}
	people, err := reach(ctx, ev.Place.ID, asked, ev.ActorInfo)
	return dgraphStruct.UUIDsOf(people), err
}

// withinOf is those of ids among within, in ids' order, or ids when within
// is nil (nothing bounds them). Pure.
func withinOf(ids, within []string) []string {
	if within == nil {
		return ids
	}
	in := make(map[string]bool, len(within))
	for _, id := range within {
		in[key(id)] = true
	}
	var out []string
	for _, id := range ids {
		if in[key(id)] {
			out = append(out, id)
		}
	}
	return out
}

func (liveInputs) Keywords(ctx context.Context, p Place, text, writer string) ([]string, error) {
	if p.Kind != PlaceChannel || keywordReach == nil {
		return nil, nil
	}
	return keywordReach(ctx, p.ID, text, writer), nil
}

func (liveInputs) VIPs(ctx context.Context, sender string, ids []string) (map[string]bool, error) {
	return alertModels.WhoseVIP(ctx, sender, ids)
}

// Held is the push gate's own answer (domain/UserFCMToken ReachableNow: pause,
// quiet hours and schedule, focus time), and for those it holds, until when
// and why as business/Notification HeldFor reads their preferences.
func (liveInputs) Held(ctx context.Context, ids []string, now time.Time) (map[string]notificationBusiness.HeldStatus, error) {
	reachable, _, err := fcmDomain.ReachableNow(ctx, ids)
	if err != nil {
		return nil, err
	}
	free := make(map[string]bool, len(reachable))
	for _, id := range reachable {
		free[key(id)] = true
	}
	var held []uuid.UUID
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil && !free[key(id)] {
			held = append(held, u)
		}
	}
	out := make(map[string]notificationBusiness.HeldStatus, len(held))
	if len(held) == 0 {
		return out, nil
	}
	prefs, err := prefDomain.LoadByUserIDs(ctx, held)
	if err != nil {
		return nil, err
	}
	for _, u := range held {
		st := notificationBusiness.HeldFor(prefs[u], now)
		if st.Reason == "" {
			st.Reason = "held"
		}
		out[key(u.String())] = st
	}
	return out, nil
}

// key is a user id as the inputs key it.
func key(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
