package domain

// What a post's side effects read and write when they run from the outbox
// (business/Post postSinks.go, chat A2): the post as it is now, read by its id,
// and its search entry written so that a second write of it is the same entry.

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

// ErrPostGone is a post that isn't there, or no longer is.
var ErrPostGone = errors.New("domain/Post: the post isn't there")

// sinkPostQuery is everything a post's side effects say about it: its words
// and files, who wrote it, its channel, what it quotes and whom it mentions.
// The mentions are read under a name of their own (post_mentioned):
// post_mentions is a list in the graph ([uid]), which DgraphPost's one
// Mention can't take, so a post that mentions anyone failed to read at all.
const sinkPostQuery = `query PostInfo($id: string){
	postInfo(func: eq(post_uuid, $id)) {
		uid
		post_uuid
		post_text
		post_kind
		post_system
		post_copy_of
		post_created_at
		post_updated_at
		post_deleted_at
		post_attachments {
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
		post_by {
			uid
			user_uuid
			user_name
			user_full_name
			user_profile_object_key
			is_bot
		}
		post_channel {
			uid
			ch_uuid
			ch_name
		}
		post_reply_to {
			uid
			post_uuid
			post_text
			post_created_at
			post_deleted_at
			post_by {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
			}
		}
		post_mentioned: post_mentions {
			mention_users {
				uid
				user_uuid
			}
		}
	}
}`

// ReadPostForSinks is the post as it is now, for a side effect about to run:
// ErrPostGone when there is no such post, and the post with its deletion time
// when it was deleted (the caller skips it).
func ReadPostForSinks(ctx context.Context, postUUID string) (*dgraphStruct.DgraphPost, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("domain/Post: no graph connection")
	}
	var raw json.RawMessage
	if err := dgraphInit.ReadInto(ctx, sinkPostQuery, map[string]string{"$id": postUUID}, &raw); err != nil {
		return nil, err
	}
	return parseSinkPost(raw)
}

// sinkPost is a post as sinkPostQuery answers it: the people it mentions
// arrive as a list of Mention nodes.
type sinkPost struct {
	dgraphStruct.DgraphPost
	Mentioned []struct {
		Users []*dgraphStruct.DgraphUser `json:"mention_users"`
	} `json:"post_mentioned"`
}

// parseSinkPost reads sinkPostQuery's answer into the post, its mentions
// gathered into one Mention as its writer holds them; ErrPostGone when it
// found none. Pure.
func parseSinkPost(raw []byte) (*dgraphStruct.DgraphPost, error) {
	var answer struct {
		PostInfo []sinkPost `json:"postInfo"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	if len(answer.PostInfo) == 0 {
		return nil, ErrPostGone
	}
	s := answer.PostInfo[0]
	p := s.DgraphPost
	var users []*dgraphStruct.DgraphUser
	for _, m := range s.Mentioned {
		users = append(users, m.Users...)
	}
	if len(users) > 0 {
		p.Mentions = &dgraphStruct.DgraphMentions{Mentions: users}
	}
	return &p, nil
}

// asIndexOps turns a bulk body's creates into indexes: a create of an entry
// that is there already fails, an index writes it again. Pure.
func asIndexOps(body string) string {
	return strings.ReplaceAll(body, `{ "create" : {`, `{ "index" : {`)
}

// IndexPostWithAttachments writes a post's search entry and its files', as an
// index (a second write of the same post is the same entry), and answers an
// error when the cluster refused any of it, so the outbox tries again.
func IndexPostWithAttachments(ctx context.Context, post *openSearchStruct.OpenSearchPost, files []*dgraphStruct.DgraphAttachment) error {
	var attachments []*openSearchStruct.OpenSearchAttachment
	for _, f := range files {
		attachments = append(attachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                     f.Uuid,
			AttachmentFileName:       f.FileName,
			AttachmentByProfile:      post.PostByProfile,
			AttachmentByUserFullName: post.PostByUserFullName,
			AttachmentObjKey:         f.ObjectKey,
			AttachmentChannelUuid:    post.PostChannelUuid,
			AttachmentPostUuid:       post.Uuid,
			AttachmentChannelName:    post.PostChannelName,
			AttachmentCreatedAt:      post.PostCreatedAt,
		})
	}
	body, err := getPostWithAttachmentBulkOperationStringForOpenSearch(ctx, post, attachments)
	if err != nil {
		return err
	}
	if err := OpenSearchBulkModels.BulkIndexChecked(ctx, asIndexOps(body)); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/IndexPostWithAttachments post %s: %v", post.Uuid, err)
		return fmt.Errorf("the search entry for post %s wasn't written: %w", post.Uuid, err)
	}
	return nil
}
