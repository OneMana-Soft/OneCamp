package business

// A channel's broadcast rules as they stand for one person, for the composer
// (chat P3, D-T12 and D-M9): whether to offer @channel, @here and @everyone,
// the words that say why not, whether to ask first and with what count, and
// the channel's own "Allow @here and @channel" with whether they may change
// it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	presenceBusiness "github.com/akashc777/OneCamp/business/Presence"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Rules is a channel's broadcast rules as they stand for someone.
type Rules struct {
	// May is whether they may notify the whole channel now; Why is the
	// rule's words when they may not.
	May bool   `json:"may"`
	Why string `json:"why,omitempty"`
	// Members is how many people are in the channel, and ConfirmAt the size
	// from which the composer asks before a broadcast (0: never).
	Members   int `json:"members"`
	ConfirmAt int `json:"confirm_at"`
	// Reach is how many people each broadcast asked about would notify now
	// (never the writer or a bot): the count the composer's question names.
	Reach map[Broadcast]int `json:"reach,omitempty"`
	// MembersMay is the channel's "Allow @here and @channel" for its members,
	// Own whether the channel set it (else it is the workspace's), and
	// Manages whether they may change it.
	MembersMay bool `json:"members_may"`
	Own        bool `json:"own"`
	Manages    bool `json:"manages"`
}

// Seams for tests.
var (
	confirmAt = func(ctx context.Context) int {
		return policyBusiness.Policies.Int(ctx, policyBusiness.ChatBroadcastConfirmAt)
	}
	channelSetting = func(ctx context.Context, channel string) (policyBusiness.Info, error) {
		return policyBusiness.Admin.InfoIn(ctx, policyBusiness.ChatBroadcastMembers.Key, channelScope(channel))
	}
	setChannelSetting = func(ctx context.Context, channel string, value json.RawMessage, by policyBusiness.Changer) (policyBusiness.Info, error) {
		if value == nil {
			return policyBusiness.Admin.ClearIn(ctx, policyBusiness.ChatBroadcastMembers.Key, channelScope(channel), by)
		}
		return policyBusiness.Admin.SetIn(ctx, policyBusiness.ChatBroadcastMembers.Key, channelScope(channel), value, by)
	}
)

func channelScope(channel string) policyBusiness.Scope {
	return policyBusiness.Scope{Kind: policyBusiness.ScopeChannel, ID: channel}
}

// RulesFor is the channel ch's broadcast rules for who (ch as read for them),
// with how many each of counts would notify now.
func RulesFor(ctx context.Context, who *userModels.UserInfo, ch *dgraphStruct.DgraphChannel, counts []Broadcast) (Rules, error) {
	setting, err := channelSetting(ctx, ch.Uuid)
	if err != nil {
		return Rules{}, err
	}
	r := Rules{
		Members:   int(ch.MemberCount),
		ConfirmAt: confirmAt(ctx),
		Own:       setting.IsSet,
		Manages:   Manages(ctx, who, ch),
	}
	r.MembersMay, _ = setting.Value.(bool)
	refusal := Check(ctx, who, ch, Asked{Broadcasts: []Broadcast{Channel}})
	r.May = refusal == nil
	if refusal != nil {
		r.Why = refusal.Msg
	}
	var asked []Broadcast
	for _, b := range counts {
		if (b == Channel || b == Here || b == Everyone) && !contains(asked, b) {
			asked = append(asked, b)
		}
	}
	if len(asked) == 0 {
		return r, nil
	}
	// One read of the channel's people answers every count: @channel and
	// @everyone reach all of them (never the writer or a bot), @here those of
	// them here now.
	people, err := InChannel(ctx, ch.Uuid, Asked{Broadcasts: []Broadcast{Channel}}, who)
	if err != nil {
		return Rules{}, err
	}
	here := 0
	for _, p := range people {
		if presenceBusiness.Here(p) {
			here++
		}
	}
	r.Reach = make(map[Broadcast]int, len(asked))
	for _, b := range asked {
		if b == Here {
			r.Reach[b] = here
		} else {
			r.Reach[b] = len(people)
		}
	}
	return r, nil
}

// ParseBroadcasts is the broadcasts a comma-separated list names ("here,channel"),
// each once; anything else is passed over. Pure.
func ParseBroadcasts(list string) []Broadcast {
	var out []Broadcast
	for _, part := range strings.Split(list, ",") {
		b := Broadcast(strings.ToLower(strings.TrimSpace(part)))
		if (b == Channel || b == Here || b == Everyone) && !contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

func contains(list []Broadcast, b Broadcast) bool {
	for _, x := range list {
		if x == b {
			return true
		}
	}
	return false
}

// SettingRefusal is a change to a channel's setting refused, with the status
// and words the route answers.
type SettingRefusal struct {
	Status int
	Msg    string
}

func (r *SettingRefusal) Error() string { return r.Msg }

// SetMembersMay sets the channel ch's "Allow @here and @channel" for its
// members (allow), or with nil gives it back to the workspace's, as who (ch
// as read for them): only its managers and the workspace's admins, in the
// app. Recorded in the audit log as every policy change is.
func SetMembersMay(ctx context.Context, who *userModels.UserInfo, ch *dgraphStruct.DgraphChannel, allow *bool) (Rules, error) {
	if !Manages(ctx, who, ch) {
		return Rules{}, &SettingRefusal{Status: http.StatusForbidden, Msg: "Only the channel's managers can change who notifies all of it."}
	}
	var value json.RawMessage
	if allow != nil {
		value = json.RawMessage(fmt.Sprintf("%t", *allow))
	}
	by := policyBusiness.Changer{UserID: who.UserPostgresInfo.Id, Email: who.UserPostgresInfo.EmailID}
	if _, err := setChannelSetting(ctx, ch.Uuid, value, by); err != nil {
		if errors.Is(err, policyBusiness.ErrNotRecorded) {
			return Rules{}, &SettingRefusal{Status: http.StatusServiceUnavailable, Msg: "The change couldn't be recorded, so it wasn't made."}
		}
		return Rules{}, err
	}
	return RulesFor(ctx, who, ch, nil)
}
