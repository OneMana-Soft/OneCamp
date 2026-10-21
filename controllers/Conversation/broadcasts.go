package controllers

// A channel's broadcast rules (chat P3, B5): what the composer offers and
// asks before a broadcast, and the channel's own "Allow @here and @channel".

import (
	"errors"
	"net/http"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetBroadcastRules answers a channel's broadcast rules for the person asking:
// whether they may notify all of it (and why not), its size and the size from
// which the composer asks first, and its own setting. With
// ?reach=channel,here,everyone, how many each would notify now, for the
// question the composer asks. GET /conversations/channel/{id}/broadcasts.
// For the channel's members.
func GetBroadcastRules(w http.ResponseWriter, r *http.Request) {
	ch, ok := memberChannel(w, r)
	if !ok {
		return
	}
	rules, err := reachBusiness.RulesFor(r.Context(), who(r), ch, reachBusiness.ParseBroadcasts(r.URL.Query().Get("reach")))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetBroadcastRules")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rules})
}

// PutBroadcastRules sets the channel's "Allow @here and @channel" for its
// members, {"members": true|false}, or with null (or none) gives it back to
// the workspace's, and answers the rules as they are now. PUT
// /conversations/channel/{id}/broadcasts. For the channel's managers and the
// workspace's admins, in the app; recorded in the audit log.
func PutBroadcastRules(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Members *bool `json:"members"`
	}
	if !decode(w, r, &in) {
		return
	}
	ch, ok := memberChannel(w, r)
	if !ok {
		return
	}
	rules, err := reachBusiness.SetMembersMay(r.Context(), who(r), ch, in.Members)
	var refusal *reachBusiness.SettingRefusal
	switch {
	case errors.As(err, &refusal):
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
	case err != nil:
		answerFailure(w, r, err, "controllers/PutBroadcastRules")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rules})
	}
}

// memberChannel is the channel the route names, as read for the person
// asking, when they are in it; else it has answered: 404 for anything that
// isn't a channel they can see (a private one they're not in included), 403
// for a public channel they haven't joined.
func memberChannel(w http.ResponseWriter, r *http.Request) (*dgraphStruct.DgraphChannel, bool) {
	if chi.URLParam(r, "kind") != conversationBusiness.KindChannel {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Only a channel has broadcasts."})
		return nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a channel's id."})
		return nil, false
	}
	ctx, me := r.Context(), who(r)
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, id, me.UserDgraphInfo.Uid)
	switch {
	case errors.Is(err, channelBusiness.ErrNotFound) || (err == nil && (ch == nil || helpers.IsSoftDeleted(ch.DeletedAt))):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
		return nil, false
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/memberChannel err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load the channel.")
		return nil, false
	case ch.IsMember == 0 && helpers.Deref(ch.IsPrivate):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
		return nil, false
	case ch.IsMember == 0:
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Join the channel to post in it."})
		return nil, false
	}
	return ch, true
}

// GetMentionables answers the "@" list's server part for the conversation the
// route names (chat P3, D-M9): who is in it, those who wrote lately first,
// the writer's teams with how many of each are in it, the broadcasts and why
// they're off here, and the zones its people set. ?thread=1 is for a reply in
// one of its threads, where a broadcast reaches nobody. GET
// /conversations/{kind}/{id}/mentionables. For the conversation's people.
func GetMentionables(w http.ResponseWriter, r *http.Request) {
	ctx, me, ref := r.Context(), who(r), refOf(r)
	place := reachBusiness.Place{Kind: ref.Kind, ID: ref.ID, Thread: r.URL.Query().Get("thread") == "1"}
	switch ref.Kind {
	case conversationBusiness.KindChannel:
		ch, ok := memberChannel(w, r)
		if !ok {
			return
		}
		place.Channel = ch
	case conversationBusiness.KindDM, conversationBusiness.KindGroup:
		if _, err := conversationBusiness.Resolve(ctx, me, ref); err != nil {
			answerFailure(w, r, err, "controllers/GetMentionables")
			return
		}
		dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, me.UserDgraphInfo.Uid, ref.ID)
		if err != nil || dm == nil || dm.ParticipantIsMember == 0 {
			if err != nil {
				answerFailure(w, r, err, "controllers/GetMentionables")
			} else {
				helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
			}
			return
		}
		for _, p := range dm.Participants {
			if p != nil && p.Uuid != "" {
				place.People = append(place.People, p.Uuid)
			}
		}
	default:
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
		return
	}
	m, err := reachBusiness.MentionablesIn(ctx, me, place)
	if err != nil {
		answerFailure(w, r, err, "controllers/GetMentionables")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": m})
}
