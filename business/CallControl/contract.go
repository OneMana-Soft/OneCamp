// Package business (CallControl) is Call Control (design/calls-vs-zoom/
// PROGRAM.md §4 #49): who runs a call, what they may do to it, and doing it.
//
// WHO RUNS A CALL (D-CL-6, and the decisions of 17 Oct in PROGRAM.md §3.3):
//   - In a DM or a group everyone is equal: nobody hosts, and anyone in the
//     call may mute the others, mute everyone and ask people to unmute.
//     Nobody can remove, lock or end it for the others: they would call
//     straight back.
//   - In a channel the person who starts the call hosts it, and so do the
//     channel's managers and the workspace's admins. A host may name co-hosts.
//   - In an instant meeting the person who made it hosts it; its guests
//     never do.
//   - When the last host and co-host leave a call people are still in, the
//     member who has been in it longest becomes its host, so a call is never
//     left with nobody able to mute a noisy line.
//
// WHAT THEY MAY DO (AbilitiesOf): a host everything; a co-host everything but
// ending the call for everyone and naming co-hosts; a participant and a guest
// nothing to others. Nobody acts on someone whose part is above theirs.
//
// HOW THE CALL KNOWS. The server sets two attributes on each person in the
// room (LiveKit attributes, which tokens never let a participant change):
// AttrRole, their part, and AttrCan, what they may do, so every screen
// (guests' too) shows the right controls without asking. The server checks
// again on every action.
//
// WHAT IT NEVER DOES. It never turns anyone's microphone or camera on: they
// are asked, and they choose. Actions through an API token are refused: a
// token acts at most as a member, and running a call needs you in it.
package business

import (
	"net/http"
	"slices"
	"strings"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// Ability is something someone in a call may do to it or to the others.
type Ability string

const (
	// CanMute: turn off someone's microphone, camera or screen share.
	CanMute Ability = "mute"
	// CanMuteAll: turn off everyone's microphone but the hosts'.
	CanMuteAll Ability = "mute_all"
	// CanAskUnmute: ask someone, or everyone, to turn their microphone on.
	CanAskUnmute Ability = "ask_unmute"
	// CanRemove: put someone out of the call until it ends.
	CanRemove Ability = "remove"
	// CanLock: lock the call, so nobody new can join, and unlock it.
	CanLock Ability = "lock"
	// CanEnd: end the call for everyone in it.
	CanEnd Ability = "end"
	// CanCohost: make someone a co-host, and make a co-host a participant again.
	CanCohost Ability = "cohost"
	// CanSpotlight: show one person large on everyone's stage.
	CanSpotlight Ability = "spotlight"
	// CanMuteOnEntry: have people join the call muted.
	CanMuteOnEntry Ability = "mute_on_entry"
)

// The participant attributes the server sets.
const (
	AttrRole = "oc.role"
	AttrCan  = "oc.can"
)

// equalCall is a call where everyone is equal: a DM's or a group's. Pure.
func equalCall(kind string) bool {
	return kind == conversationBusiness.KindDM || kind == conversationBusiness.KindGroup
}

var (
	everything = []Ability{CanMute, CanMuteAll, CanAskUnmute, CanRemove, CanLock, CanEnd, CanCohost, CanSpotlight, CanMuteOnEntry}
	cohosts    = []Ability{CanMute, CanMuteAll, CanAskUnmute, CanRemove, CanLock, CanSpotlight, CanMuteOnEntry}
	equals     = []Ability{CanMute, CanMuteAll, CanAskUnmute}
)

// AbilitiesOf answers what someone may do in a call of a kind (what it
// belongs to: channel, dm, group or meeting) with their part in it. Pure.
func AbilitiesOf(kind, role string) []Ability {
	if role == model.RoleGuest {
		return nil
	}
	if equalCall(kind) {
		return equals
	}
	switch role {
	case model.RoleHost:
		return everything
	case model.RoleCohost:
		return cohosts
	}
	return nil
}

// attributesOf is what the server tells the room about someone: their part
// and what they may do. Pure.
func attributesOf(kind, role string) map[string]string {
	can := make([]string, 0, len(everything))
	for _, a := range AbilitiesOf(kind, role) {
		can = append(can, string(a))
	}
	return map[string]string{AttrRole: role, AttrCan: strings.Join(can, ",")}
}

// rank orders the parts in a call: nobody acts on someone above them. Pure.
func rank(role string) int {
	switch role {
	case model.RoleHost:
		return 3
	case model.RoleCohost:
		return 2
	case model.RoleParticipant:
		return 1
	}
	return 0
}

// The actions someone running a call may ask for.
const (
	ActMute         = "mute"           // identity; source microphone (default), camera or screen_share
	ActMuteAll      = "mute_all"       // every microphone but the hosts' and your own
	ActAskUnmute    = "ask_unmute"     // identity
	ActAskAllUnmute = "ask_all_unmute" // everyone muted
	ActRemove       = "remove"         // identity
	ActLock         = "lock"
	ActUnlock       = "unlock"
	ActEnd          = "end"
	ActMakeCohost   = "make_cohost"   // identity
	ActRemoveCohost = "remove_cohost" // identity
	ActSpotlight    = "spotlight"     // identity
	ActEndSpotlight = "end_spotlight"
	ActMuteOnEntry  = "mute_on_entry" // on
)

// abilityFor is the ability an action needs, or "" for no action we know. Pure.
func abilityFor(action string) Ability {
	switch action {
	case ActMute:
		return CanMute
	case ActMuteAll:
		return CanMuteAll
	case ActAskUnmute, ActAskAllUnmute:
		return CanAskUnmute
	case ActRemove:
		return CanRemove
	case ActLock, ActUnlock:
		return CanLock
	case ActEnd:
		return CanEnd
	case ActMakeCohost, ActRemoveCohost:
		return CanCohost
	case ActSpotlight, ActEndSpotlight:
		return CanSpotlight
	case ActMuteOnEntry:
		return CanMuteOnEntry
	}
	return ""
}

// targeted is an action done to one person. Pure.
func targeted(action string) bool {
	return slices.Contains([]string{ActMute, ActAskUnmute, ActRemove, ActMakeCohost, ActRemoveCohost, ActSpotlight}, action)
}

// Action is what someone running a call asks for.
type Action struct {
	Action   string `json:"action"`
	Identity string `json:"identity,omitempty"`
	// Source is what to mute: microphone (the default), camera or screen_share.
	Source string `json:"source,omitempty"`
	// On is for mute_on_entry: turn it on, or off.
	On bool `json:"on,omitempty"`
}

// Result is what an action did.
type Result struct {
	// Affected is how many people it reached: one, or for mute_all and
	// ask_all_unmute, everyone it muted or asked.
	Affected int `json:"affected"`
}

// Refusal is an action that wasn't done, in words the person can act on, with
// the status a request for it is answered with.
type Refusal struct {
	Status int
	Code   string
	Msg    string
}

func (r *Refusal) Error() string { return r.Msg }

// The refusals.
var (
	ErrUnknownAction = &Refusal{http.StatusBadRequest, "unknown_action", "That isn't something you can do in a call."}
	ErrNoTarget      = &Refusal{http.StatusBadRequest, "no_target", "Say who it's for."}
	ErrThroughToken  = &Refusal{http.StatusForbidden, "not_through_token", "Running a call needs you in it. An API token can't do it."}
	ErrNotInCall     = &Refusal{http.StatusForbidden, "not_in_call", "Join the call first."}
	ErrNotAllowed    = &Refusal{http.StatusForbidden, "not_allowed", "Only the people running this call can do that."}
	ErrAboveYou      = &Refusal{http.StatusForbidden, "not_allowed", "They're running this call too, so you can't do that to them."}
	ErrYourself      = &Refusal{http.StatusBadRequest, "yourself", "Use your own controls for that."}
	ErrGuestCohost   = &Refusal{http.StatusBadRequest, "guest", "Guests can't run a call. Make someone from the workspace a co-host instead."}
	ErrTargetGone    = &Refusal{http.StatusNotFound, "not_in_call", "They're not in the call any more."}
	ErrCallOver      = &Refusal{http.StatusConflict, "call_over", "This call has ended."}
)
