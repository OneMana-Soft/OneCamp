// Package postdelete is a post's soft delete, shared by a person's delete
// (business/Post DeletePost) and a bot's dropped streamed post (business/BotPost
// ChannelStream.Abort, V-578). BotPost can't import business/Post (Post ->
// Webhook -> BotPost), so the delete lives below both; only the person's
// "post.deleted" webhook stays in DeletePost.
package postdelete

import (
	"context"
	"time"

	"github.com/google/uuid"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// Delete marks the post deleted in Postgres and the graph, takes its files
// with it, marks it (and its replies and files) deleted in search, and, unless
// a bulk import is running, publishes the delete and drops its AI embedding
// and the memory made from it. rawDgraphPost carries the post's uuid, its
// author (PostBy) and its files (MediaObj).
func Delete(ctx context.Context, rawDgraphPost *dgraphStruct.DgraphPost, channelId string) (err error) {

	postUUID, err := uuid.Parse(rawDgraphPost.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PostDelete Failed to parse post UUID err: %+v",
			err)
		return
	}

	currentTime := time.Now()

	err = domain.SoftDeletePostByUUIUD(ctx, postUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PostDelete Failed to soft delete post err: %+v",
			err)
		return
	}

	// Its files and its replies' go with it, before it leaves the graph: a
	// failure leaves the post there to delete again.
	_, err = attachmentBusiness.ArchiveFilesOf(ctx, attachmentBusiness.PostFiles, postUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PostDelete Failed to take its files off with it err: %+v",
			err)
		return
	}

	// add delete time in dgraph
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:       "uid(po)",
		Uuid:      postUUID.String(),
		DType:     []string{"Post"},
		DeletedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphPost(ctx, &dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PostDelete Failed to update post in dgraph err: %+v",
			err)

		return

	}

	postBy := ""
	if rawDgraphPost.PostBy != nil {
		postBy = rawDgraphPost.PostBy.Uuid
	}
	mqttPost := mqttStruct.MqttPost{
		PostUuid:        postUUID.String(),
		Type:            mqttStruct.TYPE_DELETE,
		PostChannelUuid: channelId,
		PostByUserUuid:  postBy,
	}

	openSearchPost := &openSearchStruct.OpenSearchPost{
		Uuid:          rawDgraphPost.Uuid,
		PostDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	helpers.GoSafeNamed("domain.DeletePostWithAttachmentsInOpenSearch", func() { domain.DeletePostWithAttachmentsInOpenSearch(openSearchPost, rawDgraphPost.MediaObj) })
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_post_id", "attachment_post_id"},
		rawDgraphPost.Uuid,
		currentTime.Unix(),
		[]string{"comments", "attachments"},
		"cascade",
	)

	// Skip live MQTT/AI fan-out during bulk Slack import.
	// Postgres + Dgraph + OpenSearch are already updated; we just don't
	// want to push delete events for thousands of historical messages
	// or trigger AI embedding deletions one-by-one (a re-embed pass
	// after the import handles cleanup more efficiently).
	if helpers.IsBulkImport(ctx) {
		return
	}

	helpers.GoSafeNamed("mqttBusiness.PublishPost", func() { mqttBusiness.PublishPost(&mqttPost, channelId) })

	// Delete AI embedding (async)
	helpers.GoSafeNamed("business/PostDelete:Delete", func() {
		bgCtx := context.Background()
		if err := ai.DeleteEmbedding(bgCtx, "post", rawDgraphPost.Uuid); err != nil {
			helpers.LogErrorWithContext(bgCtx, "business/PostDelete failed to delete AI embedding: %v", err)
		}
	})
	// Cascade into the Workspace Memory layer: drop any decisions /
	// commitments / questions the extractor derived from this post so
	// deleted content can't linger in memory or resurface via AI.
	ai.DeleteMemoryBySourceAsync("post", rawDgraphPost.Uuid)
	// Cascade soft-delete to AI embeddings (post + its comments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid", "post_uuid"},
		rawDgraphPost.Uuid,
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)

	return
}
