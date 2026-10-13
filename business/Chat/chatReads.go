package business

import (
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// DeletedChatText is what a deleted direct or group message's thread shows in
// its place.
const DeletedChatText = "<p>This message was deleted.</p>"

// ChatThreadForMember is a direct or group message's thread as one of its
// people opens it, as a post's is (business/Post ThreadForMember). A live
// message is the thread as read. A deleted one keeps its text, files and
// reactions in the graph, and its thread used to return them all: when
// replies to it are still there, the thread opens on a placeholder, "This
// message was deleted.", holding the replies and when it was sent, and
// nothing else of the message; with no replies there is no thread (ok is
// false). chat is read with its deletion time.
func ChatThreadForMember(chat *dgraphStruct.DgraphChat) (thread *dgraphStruct.DgraphChat, ok bool) {
	if chat == nil {
		return nil, false
	}
	if !helpers.IsSoftDeleted(chat.DeletedAt) {
		return chat, true
	}
	if len(chat.Comments) == 0 {
		return nil, false
	}
	return &dgraphStruct.DgraphChat{
		Uid:              chat.Uid,
		Uuid:             chat.Uuid,
		DM:               chat.DM,
		Body:             DeletedChatText,
		Comments:         chat.Comments,
		ChatCommentCount: chat.ChatCommentCount,
		CreatedAt:        chat.CreatedAt,
		DeletedAt:        chat.DeletedAt,
	}, true
}
