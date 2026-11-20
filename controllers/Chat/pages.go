package controllers

// A DM's and a group's pages and a message's thread (H5B). Each route asks
// business for its page in one read, the group's and the thread's gate
// inside it, and answers every refusal with the code the route has always
// given (writePageFailure).

import (
	"context"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Chat"
	messageDomain "github.com/akashc777/OneCamp/domain/Message"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// writePageFailure answers a page business refused: someone not in the
// conversation a 403; a group that isn't there, or a DM with no one, a 400
// saying missing; any other failure a 400.
func writePageFailure(ctx context.Context, w http.ResponseWriter, route, missing string, err error) {
	switch {
	case errors.Is(err, business.ErrNotInConversation):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not authorised"})
	case errors.Is(err, business.ErrNoConversation), errors.Is(err, business.ErrNoSuchPerson):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": missing})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/%s Failed to get chats err: %+v", route, err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get chats", "err": err})
	}
}

// writePage answers a page read, or its refusal.
func writePage(ctx context.Context, w http.ResponseWriter, route, missing string, chats business.ChatPagination, err error) {
	if err != nil {
		writePageFailure(ctx, w, route, missing, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})
}

// groupPageRoute answers a group's older or newer page: its grp_id, and its
// edge from time_stamp (helpers.PageEdge).
func groupPageRoute(w http.ResponseWriter, r *http.Request, route string, dir messageDomain.Direction) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	grpIdString := chi.URLParam(r, "grp_id")
	edge, err := helpers.PageEdgeOf(r, chi.URLParam(r, "time_stamp"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	if len(grpIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse grpId string"})
		return
	}
	chats, err := business.GroupPage(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, dir, edge)
	writePage(ctx, w, route, "Failed to get grp chat info", chats, err)
}

// dmPageRoute answers a DM's older or newer page: its user_uuid, and its
// edge from time_stamp.
func dmPageRoute(w http.ResponseWriter, r *http.Request, route string, dir messageDomain.Direction) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	secondUserUUIDString := chi.URLParam(r, "user_uuid")
	edge, err := helpers.PageEdgeOf(r, chi.URLParam(r, "time_stamp"))
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	if _, err := uuid.Parse(secondUserUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to pas userUUID string", "err": err})
		return
	}
	chats, err := business.DMPage(ctx, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString, dir, edge)
	writePage(ctx, w, route, "Failed to get user info", chats, err)
}

func GetOldGroupChats(w http.ResponseWriter, r *http.Request) {
	groupPageRoute(w, r, "GetOldGroupChats", messageDomain.Older)
}

func GetNewGroupChats(w http.ResponseWriter, r *http.Request) {
	groupPageRoute(w, r, "GetNewGroupChats", messageDomain.Newer)
}

func GetOldChats(w http.ResponseWriter, r *http.Request) {
	dmPageRoute(w, r, "GetOldChats", messageDomain.Older)
}

func GetNewChats(w http.ResponseWriter, r *http.Request) {
	dmPageRoute(w, r, "GetNewChats", messageDomain.Newer)
}

// GetNewGroupChatsIncludingChat is a group's page that opens at a message.
// Who may read the group is answered before anything about the message, as
// it always was: a non-member is refused 403 whatever the message, so a
// message that can't be read asks the gate before its 400.
func GetNewGroupChatsIncludingChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	grpIdString := chi.URLParam(r, "grp_id")
	chatUUIDString := chi.URLParam(r, "chat_uuid")
	if chatUUIDString == "" || grpIdString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse chatUUID ot grpId string"})
		return
	}
	at, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)
	if err != nil || at == nil || at.CreatedAt == nil {
		if _, gate := business.GroupPage(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, messageDomain.Latest, helpers.PageEdge{}); gate != nil {
			writePageFailure(ctx, w, "GetNewGroupChatsIncludingChat", "Failed to get group chat", gate)
			return
		}
		writePageFailure(ctx, w, "GetNewGroupChatsIncludingChat", "Failed to get group chat", errors.Join(errors.New("the message can't be read"), err))
		return
	}
	chats, err := business.GroupPage(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, messageDomain.NewerFrom, helpers.PageEdge{At: *at.CreatedAt})
	writePage(ctx, w, "GetNewGroupChatsIncludingChat", "Failed to get group chat", chats, err)
}

// GetNewChatsIncludingChat is a DM's page that opens at a message.
func GetNewChatsIncludingChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	secondUserUUIDString := chi.URLParam(r, "user_uuid")
	chatUUIDString := chi.URLParam(r, "chat_uuid")
	if chatUUIDString == "" || secondUserUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse chatUUID string"})
		return
	}
	if _, err := uuid.Parse(secondUserUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to pas userUUID string", "err": err})
		return
	}
	at, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)
	if err != nil || at == nil || at.CreatedAt == nil {
		writePageFailure(ctx, w, "GetNewChatsIncludingChat", "Failed to get chats", errors.Join(errors.New("the message can't be read"), err))
		return
	}
	chats, err := business.DMPage(ctx, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString, messageDomain.NewerFrom, helpers.PageEdge{At: *at.CreatedAt})
	writePage(ctx, w, "GetNewChatsIncludingChat", "Failed to get chats", chats, err)
}

// GetLatestGroupChats opens a group: its latest page, gated in its read,
// then marked read (business.OpenGroup).
func GetLatestGroupChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	grpIdString := chi.URLParam(r, "grp_id")
	if len(grpIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse grpId string"})
		return
	}
	chats, err := business.OpenGroup(ctx, userInfo.UserPostgresInfo.Id, userInfo.UserDgraphInfo.Uid, grpIdString)
	writePage(ctx, w, "GetLatestGroupChats", "Failed to get user info", chats, err)
}

// GetLatestChats opens a DM: its latest page, the other person asked for
// only when it is empty, then marked read (business.OpenDM).
func GetLatestChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	secondUserUUIDString := chi.URLParam(r, "user_uuid")
	if _, err := uuid.Parse(secondUserUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to pas userUUID string", "err": err})
		return
	}
	chats, err := business.OpenDM(ctx, userInfo.UserPostgresInfo.Id, secondUserUUIDString)
	writePage(ctx, w, "GetLatestChats", "Failed to get user info", chats, err)
}

// GetDgraphChatByUUIDWithAllComments opens a message's thread, the gate asked
// in the thread's own read (business.MemberThread). A message that isn't
// there, or isn't in a conversation, is a 404, as a deleted one is; someone
// not in its conversation a 403; a graph that doesn't answer a 503 the page
// can try again (helpers.WriteReadFailure).
func GetDgraphChatByUUIDWithAllComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	dgraphChat, err := business.MemberThread(ctx, chi.URLParam(r, "chat_uuid"), userInfo.UserDgraphInfo.Uid)
	if errors.Is(err, business.ErrNotInConversation) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You're not in this conversation."})
		return
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "thread", "controllers/GetDgraphChatByUUIDWithAllComments")
		return
	}
	// A deleted message's thread shows nothing of it: a placeholder over its
	// replies, or no thread when there are none (business.ChatThreadForMember).
	thread, ok := business.ChatThreadForMember(dgraphChat)
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got reaction comment successfully!", "data": thread})
}
