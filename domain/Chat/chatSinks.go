package domain

// What a DM's or group message's side effects read and write when they run
// from the outbox (business/Chat chatSinks.go, chat A2): the message as it is
// now, read by its id with its conversation's people, and its search entry
// written so that a second write of it is the same entry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
)

// ErrChatGone is a message that isn't there, or no longer is.
var ErrChatGone = errors.New("domain/Chat: the message isn't there")

// sinkChatQuery is everything a message's side effects say about it: its words
// and files, who wrote it and to whom, its conversation and that
// conversation's people (as the group's own read shows them: dmParticipantVisible),
// what it quotes (as resolveReplyParentChat reads it) and whom it mentions.
// The mentions are read under a name of their own (chat_mentioned):
// chat_mentions is a list in the graph ([uid]), which DgraphChat's one
// Mention can't take (a channel post's read failed so, business/Post).
const sinkChatQuery = `query ChatInfo($id: string){
	chatInfo(func: eq(chat_uuid, $id)) {
		uid
		chat_uuid
		chat_body_text
		chat_kind
		chat_system
		chat_copy_of
		chat_created_at
		chat_updated_at
		chat_deleted_at
		chat_attachments {
			attachment_uuid
			attachment_file_name
			attachment_obj_key
			attachment_type
			attachment_raw_type
			attachment_width
			attachment_height
			attachment_size
			attachment_duration
		}
		chat_from {
			uid
			user_uuid
			user_name
			user_full_name
			user_profile_object_key
			is_bot
		}
		chat_to {
			uid
			user_uuid
			user_name
			user_full_name
			user_profile_object_key
			is_bot
		}
		chat_dm {
			uid
			dm_grouping_id
			dm_participants @filter(` + dmParticipantVisible + `) {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
			}
		}
		chat_reply_to {
			uid
			chat_from {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
				user_deleted_at
			}
			chat_to {
				uid
				user_uuid
				user_name
				user_profile_object_key
				is_bot
				user_deleted_at
			}
			chat_dm {
				dm_grouping_id
			}
			chat_created_at
			chat_updated_at
			chat_deleted_at
			chat_body_text
			chat_attachments {
				attachment_file_name
				attachment_obj_key
			}
			chat_uuid
		}
		chat_mentioned: chat_mentions {
			mention_users {
				uid
				user_uuid
			}
		}
	}
}`

// ReadChatForSinks is the message as it is now, for a side effect about to
// run: ErrChatGone when there is no such message, and the message with its
// deletion time when it was deleted (the caller skips it).
func ReadChatForSinks(ctx context.Context, chatUUID string) (*dgraphStruct.DgraphChat, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("domain/Chat: no graph connection")
	}
	var raw json.RawMessage
	if err := dgraphInit.ReadInto(ctx, sinkChatQuery, map[string]string{"$id": chatUUID}, &raw); err != nil {
		return nil, err
	}
	return parseSinkChat(raw)
}

// sinkChat is a message as sinkChatQuery answers it: the people it mentions
// arrive as a list of Mention nodes.
type sinkChat struct {
	dgraphStruct.DgraphChat
	Mentioned []struct {
		Users []*dgraphStruct.DgraphUser `json:"mention_users"`
	} `json:"chat_mentioned"`
}

// parseSinkChat reads sinkChatQuery's answer into the message, its mentions
// gathered into one Mention as its writer holds them; ErrChatGone when it
// found none. Pure.
func parseSinkChat(raw []byte) (*dgraphStruct.DgraphChat, error) {
	var answer struct {
		ChatInfo []sinkChat `json:"chatInfo"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	if len(answer.ChatInfo) == 0 {
		return nil, ErrChatGone
	}
	s := answer.ChatInfo[0]
	c := s.DgraphChat
	var users []*dgraphStruct.DgraphUser
	for _, m := range s.Mentioned {
		users = append(users, m.Users...)
	}
	if len(users) > 0 {
		c.Mentions = &dgraphStruct.DgraphMentions{Mentions: users}
	}
	return &c, nil
}

// asChatIndexOps turns a bulk body's creates into indexes: a create of an
// entry that is there already fails, an index writes it again. Pure.
func asChatIndexOps(body string) string {
	return strings.ReplaceAll(body, `{ "create" : {`, `{ "index" : {`)
}

// IndexChatWithAttachments writes a message's search entry and its files', as
// an index (a second write of the same message is the same entry), and answers
// an error when the cluster refused any of it, so the outbox tries again.
func IndexChatWithAttachments(ctx context.Context, chat *openSearchStruct.OpenSearchChat, files []*dgraphStruct.DgraphAttachment) error {
	var attachments []*openSearchStruct.OpenSearchAttachment
	for _, f := range files {
		attachments = append(attachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                           f.Uuid,
			AttachmentFileName:             f.FileName,
			AttachmentByProfile:            chat.ChatByProfile,
			AttachmentByUserFullName:       chat.ChatByUserFullName,
			AttachmentObjKey:               f.ObjectKey,
			AttachmentChatGrpId:            chat.ChatGrpId,
			AttachmentChatUuid:             chat.Uuid,
			AttachmentCreatedAt:            chat.ChatCreatedAt,
			AttachmentChatFromUserUuid:     chat.ChatByUserUuid,
			AttachmentChatFromUserFullName: chat.ChatByUserFullName,
			AttachmentChatParticipants:     chat.ChatParticipants,
			AttachmentDeletedAt:            nil,
		})
	}
	body, err := getChatWithAttachmentBulkOperationStringForOpenSearch(ctx, chat, attachments)
	if err != nil {
		return err
	}
	if err := OpenSearchBulkModels.BulkIndexChecked(ctx, asChatIndexOps(body)); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/IndexChatWithAttachments chat %s: %v", chat.Uuid, err)
		return fmt.Errorf("the search entry for message %s wasn't written: %w", chat.Uuid, err)
	}
	return nil
}
