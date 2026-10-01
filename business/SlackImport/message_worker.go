package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// messageContext is the per-chunk scratch space for the worker.
//
// It caches OneCamp dgraph data needed when creating posts so we don't
// hammer Dgraph with one round-trip per message. Cache lifetime is the
// chunk; for a daily file with thousands of messages the savings are
// substantial.
type messageContext struct {
	importId  uuid.UUID
	chunkId   uuid.UUID
	chunkType string

	// workspaceName is the same as the parent job's workspace_name,
	// cached on the context so the message worker doesn't refetch the
	// job row per message just to consult slack_workspace_id_map.
	workspaceName string

	channelSlackId string
	channelUUID    uuid.UUID
	dgraphChannel  *dgraphStruct.DgraphChannel
	channelName    string

	// Slack user id -> resolved OneCamp user info. Populated lazily.
	userCache map[string]*userModels.UserInfo

	// Slack user id -> display name (for mention rendering fallback).
	userNameCache map[string]string

	// Slack channel id -> {uuid, name} (for <#C123> resolution).
	channelCache map[string]channelRef

	// Last successfully imported Slack ts (high water mark for resume).
	lastCursor string

	// Items committed in this chunk run.
	itemsDone int

	// File IDs we've already enqueued during this chunk. Prevents
	// duplicate downloads if the same file is referenced in multiple
	// messages of the same daily file.
	enqueuedFiles map[string]struct{}

	// Pending file-chunk inserts buffered for batch flush at chunk end.
	// scheduleFileChunk appends here; flushMessageContext drains it via
	// importModels.CreateChunks in a single round-trip per chunk instead
	// of N. Safe because the chunks-table unique index swallows any
	// duplicates we might race with another worker.
	pendingFileChunks []*importModels.Chunk
}

type channelRef struct {
	uuid uuid.UUID
	name string
}

// processMessageChunk imports every top-level message in a single
// per-day Slack JSON file. Threads (replies) are deferred to the
// channel_threads pass so we can guarantee the parent always exists
// before its replies.
//
// Idempotency layers:
//   - Per-import: every message is recorded in slack_import_id_map; on
//     retry we skip already-imported messages. last_cursor on the chunk
//     speeds the resume path so we don't re-query id_map per message.
//   - Cross-import: slack_workspace_id_map (populated by prior imports
//     of the same workspace) is consulted before any insert. A re-export
//     of the same workspace skips every previously-imported message.
//
// Heartbeat: every 250 messages we call HeartbeatChunk so the reaper
// knows we're alive on a multi-thousand-message daily file.
func processMessageChunk(ctx context.Context, arc *Archive, chunk *importModels.Chunk,
	skipSubtypes bool, importingUser *userModels.UserInfo, workspaceName string) error {

	if chunk.ObjectKey == nil || chunk.ChannelSlackId == nil {
		return fmt.Errorf("message chunk missing object_key/channel_slack_id")
	}

	zf, err := OpenInZip(arc.Reader, *chunk.ObjectKey)
	if err != nil {
		return fmt.Errorf("open zip entry %s: %w", *chunk.ObjectKey, err)
	}

	mc, err := newMessageContext(ctx, chunk, workspaceName)
	if err != nil {
		return err
	}
	defer flushMessageContext(ctx, mc)

	// Resume cursor: skip messages with ts <= last_cursor on retry.
	resumeCursor := ""
	if chunk.LastCursor != nil {
		resumeCursor = *chunk.LastCursor
	}

	count := 0
	heartbeatEvery := 250
	committed := 0

	err = IterMessages(ctx, arc, zf, func(m *SlackMessage) bool {
		count++

		// Cooperative cancellation check (cheap).
		select {
		case <-ctx.Done():
			return false
		default:
		}

		// Skip resumed messages.
		if resumeCursor != "" && tsLE(m.Ts, resumeCursor) {
			return true
		}

		// Skip system noise.
		if skipSubtypes && SubtypeIsSystem(m.Subtype) {
			return true
		}
		if SubtypeIsTombstone(m.Subtype) {
			// Slack's `message_deleted` carries a `previous_message.ts`.
			// We honour deletes by tombstoning the OneCamp post if we
			// have it mapped. Idempotent: re-runs are no-ops.
			if m.PreviousMessage != nil && m.PreviousMessage.Ts != "" {
				_ = applyMessageDelete(ctx, mc, m.PreviousMessage.Ts)
			}
			return true
		}
		if m.Subtype == "message_changed" {
			// Edit. Re-render the post body and update the OneCamp row.
			// We do NOT promote the edit timestamp (m.Ts) into the
			// id_map; the original ts (in SubMessage.Ts) is the canonical
			// id. Empty SubMessage = malformed export, skip.
			if m.SubMessage != nil && m.SubMessage.Ts != "" {
				_ = applyMessageEdit(ctx, mc, m.SubMessage)
			}
			return true
		}

		// Top-level pass: only messages that are NOT thread replies.
		// A message is a thread reply iff thread_ts != "" and != ts.
		if mc.chunkType == importModels.ChunkChannelMessages &&
			m.ThreadTs != "" && m.ThreadTs != m.Ts {
			return true
		}

		// Threads pass: only messages that ARE thread replies.
		if mc.chunkType == importModels.ChunkChannelThreads &&
			(m.ThreadTs == "" || m.ThreadTs == m.Ts) {
			return true
		}

		// Idempotent skip — same import retry. We still process file
		// enqueue (below) before this short-circuit so a chunk that
		// previously crashed AFTER message creation but BEFORE file
		// scheduling can recover on retry. scheduleFileChunk relies on
		// the chunks-table unique constraint to swallow duplicates from
		// the happy path.
		entityType := importModels.EntityMessage
		if mc.chunkType == importModels.ChunkChannelThreads {
			entityType = importModels.EntityComment
		}
		alreadyMapped := false
		if existing, _ := importModels.LookupIdMapping(ctx, mc.importId, entityType, m.Ts); existing != uuid.Nil {
			alreadyMapped = true
		}

		// Enqueue file chunks for any attachments referenced by this
		// message. Done EARLY so retries after a partial-success message
		// (created in PG/Dgraph but file-chunk insert failed) still get
		// the files queued. The chunks table's uq_slack_chunks_logical
		// index makes scheduleFileChunk a no-op for already-queued files.
		// We buffer here and batch-insert at the end of the chunk loop
		// to avoid one round-trip per file on hot daily files.
		if len(m.Files) > 0 {
			for _, sf := range m.Files {
				if _, seen := mc.enqueuedFiles[sf.ID]; seen {
					continue
				}
				mc.enqueuedFiles[sf.ID] = struct{}{}
				if c := buildFileChunk(mc.importId, mc.channelSlackId, m.Ts, sf); c != nil {
					mc.pendingFileChunks = append(mc.pendingFileChunks, c)
				}
			}
			// Flush in batches of 200 so a giant daily file doesn't
			// build up an unbounded slice in memory.
			if len(mc.pendingFileChunks) >= 200 {
				flushFileChunks(ctx, mc.pendingFileChunks)
				mc.pendingFileChunks = mc.pendingFileChunks[:0]
			}
		}

		if alreadyMapped {
			mc.lastCursor = m.Ts
			return true
		}

		// Cross-import dedup. A re-export of the same workspace contains
		// every message including the ones we already imported. Skip
		// silently and just record the per-import mapping for activity
		// counting + rollback safety.
		if mc.workspaceName != "" {
			if existing, _ := importModels.LookupWorkspaceMapping(ctx, mc.workspaceName, entityType, m.Ts); existing != uuid.Nil {
				_ = importModels.UpsertIdMappingWithOwnership(ctx, mc.importId, entityType, m.Ts,
					existing, nil, mustMarshal(map[string]interface{}{"matched_by": "workspace_map"}),
					false /* not created by this import */)
				mc.lastCursor = m.Ts
				return true
			}
		}

		var importErr error
		if mc.chunkType == importModels.ChunkChannelMessages {
			importErr = importTopLevelMessage(ctx, mc, m, importingUser)
		} else {
			importErr = importThreadReply(ctx, mc, m, importingUser)
		}

		if importErr != nil {
			importModels.LogImportError(ctx, mc.importId, &mc.chunkId,
				entityType, m.Ts,
				importModels.SeverityError, "MESSAGE_IMPORT_FAILED",
				importErr.Error(),
				mustMarshal(map[string]interface{}{"subtype": m.Subtype}))
			// Continue with the rest of the file. One bad message
			// shouldn't kill the whole chunk; the chunk is marked
			// failed only if a fatal infra error fires below.
			return true
		}

		mc.itemsDone++
		mc.lastCursor = m.Ts
		committed++

		// Heartbeat to keep the reaper happy on long files.
		if committed%heartbeatEvery == 0 {
			cursor := mc.lastCursor
			_ = importModels.HeartbeatChunk(ctx, mc.chunkId, mc.itemsDone, &cursor)
		}

		return true
	})

	if err != nil {
		return err
	}

	cursor := mc.lastCursor
	return importModels.FinishChunk(ctx, mc.chunkId, mc.itemsDone, &cursor)
}

// importTopLevelMessage creates a Post in the OneCamp channel for one
// non-thread Slack message. It handles user resolution, mrkdwn rendering,
// and the underlying Postgres+Dgraph+OpenSearch+MQTT fan-out via the
// existing postBusiness.CreatePost.
func importTopLevelMessage(ctx context.Context, mc *messageContext, m *SlackMessage,
	importingUser *userModels.UserInfo) error {

	authorInfo, err := mc.resolveAuthor(ctx, m, importingUser)
	if err != nil {
		return fmt.Errorf("resolve author: %w", err)
	}

	html := renderMessageHTML(ctx, mc, m)
	if html == "" && len(m.Files) == 0 {
		// Nothing to import. Mark mapped so retry won't re-enter.
		return importModels.UpsertIdMapping(ctx, mc.importId, importModels.EntityMessage, m.Ts,
			uuid.New(), nil, mustMarshal(map[string]interface{}{"empty": true}))
	}

	// CreatePost mints a uuid internally; we capture it post-call from
	// the OutputCreatePostForPost.Uuid field. The import_id_map upsert
	// happens AFTER successful creation so a failure halfway through
	// doesn't poison the map. This means a crash leaves the message in
	// a "ready to retry" state on next run.
	postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    html,
		ChannelUuid: mc.channelUUID.String(),
		ChannelUUID: mc.channelUUID,
	}

	// Backdate context so CreatePost stamps Dgraph + OpenSearch with the
	// original Slack ts instead of time.Now(). The Postgres backfill below
	// covers the PG row separately because INSERT … DEFAULT NOW() ignores
	// the ctx-scoped override.
	slackTime := slackTsToTime(m.Ts)
	ctxBackdated := helpers.WithImportTimestamp(ctx, slackTime)

	created, err := postBusiness.CreatePost(ctxBackdated, postInfo, authorInfo, nil, mc.dgraphChannel)
	if err != nil {
		return fmt.Errorf("CreatePost: %w", err)
	}

	postUUID, err := uuid.Parse(created.Uuid)
	if err != nil {
		return fmt.Errorf("parse new post uuid: %w", err)
	}

	// Postgres-only backfill. Dgraph + OpenSearch were already stamped
	// with the slack ts via the backdated context above.
	if err := backdatePost(ctx, postUUID, slackTime); err != nil {
		// Non-fatal — the post exists, the timestamp is just slightly off.
		helpers.LogWarnWithContext(ctx,
			"SlackImport could not backdate post %s ts=%s: %+v", postUUID, m.Ts, err)
	}

	if err := importModels.UpsertIdMappingWithOwnership(ctx, mc.importId, importModels.EntityMessage, m.Ts,
		postUUID, nil,
		mustMarshal(map[string]interface{}{
			"channel_slack_id": mc.channelSlackId,
			"author_slack":     m.User,
			"subtype":          m.Subtype,
		}), true /* physically created */); err != nil {
		return err
	}
	// Promote into workspace map so the next re-import skips this message.
	if mc.workspaceName != "" {
		_ = importModels.UpsertWorkspaceMapping(ctx, mc.workspaceName,
			importModels.EntityMessage, m.Ts, postUUID, mc.importId)
	}
	return nil
}

// importThreadReply attaches the message as a comment to its parent
// channel post. DM/MPIM thread replies are handled separately by
// dm_worker.importDMThreadReply because they hit the chat schema, not
// posts.
//
// Parent resolution: look up parent message's slack ts in the id_map.
// If the parent isn't there yet (e.g., the threads pass started before
// the corresponding top-level pass finished — should not happen because
// the orchestrator waits, but we defend) we log and skip.
func importThreadReply(ctx context.Context, mc *messageContext, m *SlackMessage,
	importingUser *userModels.UserInfo) error {

	parentUUID, err := importModels.LookupIdMapping(ctx, mc.importId, importModels.EntityMessage, m.ThreadTs)
	if err != nil {
		return err
	}
	if parentUUID == uuid.Nil {
		importModels.LogImportError(ctx, mc.importId, &mc.chunkId,
			importModels.EntityComment, m.Ts,
			importModels.SeverityWarning, "THREAD_PARENT_NOT_FOUND",
			"thread reply skipped because parent message was not imported",
			mustMarshal(map[string]interface{}{"thread_ts": m.ThreadTs}))
		return nil
	}

	authorInfo, err := mc.resolveAuthor(ctx, m, importingUser)
	if err != nil {
		return fmt.Errorf("resolve author: %w", err)
	}

	// Need the dgraph post info for the comment business call.
	dgraphPost, err := postBusiness.GetUnDeletedDgraphPostChannelBasicInfo(ctx, parentUUID.String())
	if err != nil || dgraphPost == nil {
		return fmt.Errorf("get parent dgraph post: %w", err)
	}

	html := renderMessageHTML(ctx, mc, m)
	if html == "" && len(m.Files) == 0 {
		return importModels.UpsertIdMapping(ctx, mc.importId, importModels.EntityComment, m.Ts,
			uuid.New(), &m.ThreadTs, mustMarshal(map[string]interface{}{"empty": true}))
	}

	commentInfo := &postAdapter.InputCreateOrUpdateCommentToPost{
		HTMLText: html,
		PostUuid: parentUUID.String(),
	}

	// Backdate Dgraph + OpenSearch via context; Postgres handled below.
	slackTime := slackTsToTime(m.Ts)
	ctxBackdated := helpers.WithImportTimestamp(ctx, slackTime)

	created, err := postBusiness.CreatePostComment(ctxBackdated, commentInfo, authorInfo, nil, dgraphPost)
	if err != nil {
		return fmt.Errorf("CreatePostComment: %w", err)
	}

	// CreatePostComment returns the new comment uuid in OutputCreatePostCommentForPost.Uuid.
	commentUUID, err := uuid.Parse(created.Uuid)
	if err != nil {
		// Fall back: still record mapping so retry doesn't dupe.
		return importModels.UpsertIdMapping(ctx, mc.importId, importModels.EntityComment, m.Ts,
			uuid.New(), &m.ThreadTs, mustMarshal(map[string]interface{}{"unparsed_uuid": created.Uuid}))
	}

	if err := backdateComment(ctx, commentUUID, slackTime); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport could not backdate comment %s ts=%s: %+v", commentUUID, m.Ts, err)
	}

	if err := importModels.UpsertIdMappingWithOwnership(ctx, mc.importId, importModels.EntityComment, m.Ts,
		commentUUID, &m.ThreadTs,
		mustMarshal(map[string]interface{}{
			"author_slack": m.User,
			"thread_ts":    m.ThreadTs,
		}), true); err != nil {
		return err
	}
	if mc.workspaceName != "" {
		_ = importModels.UpsertWorkspaceMapping(ctx, mc.workspaceName,
			importModels.EntityComment, m.Ts, commentUUID, mc.importId)
	}
	return nil
}

// renderMessageHTML produces the final HTML for a Slack message,
// resolving mentions/channel-links via the message context cache.
//
// Bot prefixing: messages posted by bots (subtype="bot_message" or any
// message with a non-empty bot_id and no real user) get a "[bot:<name>]"
// chip prepended so the imported timeline preserves the distinction
// between human and bot content. Slack's exports include the bot's
// display name in either bot_profile.name or username.
func renderMessageHTML(ctx context.Context, mc *messageContext, m *SlackMessage) string {
	resolvers := mrkdwnResolvers{
		resolveUser: func(slackId string) (string, string, bool) {
			ocUUID, err := importModels.LookupIdMapping(ctx, mc.importId, importModels.EntityUser, slackId)
			if err != nil || ocUUID == uuid.Nil {
				if name, ok := mc.userNameCache[slackId]; ok {
					return "", name, false
				}
				return "", "", false
			}
			name := mc.userNameCache[slackId]
			return ocUUID.String(), name, true
		},
		resolveChannel: func(slackId string) (string, string, bool) {
			if r, ok := mc.channelCache[slackId]; ok {
				return r.uuid.String(), r.name, true
			}
			ocUUID, err := importModels.LookupIdMapping(ctx, mc.importId, importModels.EntityChannel, slackId)
			if err != nil || ocUUID == uuid.Nil {
				return "", "", false
			}
			// Resolve the human-readable channel name from id_map metadata so
			// we don't fall back to a bare # symbol. Cached on the context so
			// repeated mentions in the same chunk don't re-query Postgres.
			name := ""
			if md, _ := importModels.GetIdMapMetadata(ctx, mc.importId, importModels.EntityChannel, slackId); len(md) > 0 {
				var meta struct {
					FinalName string `json:"final_name"`
				}
				_ = jsonUnmarshalRaw(md, &meta)
				name = meta.FinalName
			}
			mc.channelCache[slackId] = channelRef{uuid: ocUUID, name: name}
			return ocUUID.String(), name, true
		},
	}
	body := RenderSlackText(ctx, m, resolvers)

	// Bot attribution prefix. We add it as a styled <span> so the FE can
	// render it visually distinct without a schema change. Empty string
	// stays empty so the "no body, no files" branch in the caller still
	// fires correctly.
	if isBotMessage(m) && body != "" {
		botName := pickBotName(m)
		if botName == "" {
			botName = "bot"
		}
		body = `<span class="mention bot-attrib" data-id="bot@` +
			htmlEscape(botName) + `">[bot: ` + htmlEscape(botName) + `]</span> ` + body
	}
	return body
}

// isBotMessage detects bot-authored messages across the various shapes
// Slack exports use. A message is considered a bot post when:
//   - subtype == "bot_message", OR
//   - bot_id is present and user is empty (some workflows send as bot).
func isBotMessage(m *SlackMessage) bool {
	if m.Subtype == "bot_message" {
		return true
	}
	if m.BotID != "" && m.User == "" {
		return true
	}
	return false
}

// pickBotName returns the most descriptive bot identifier available.
// Falls back through bot_profile.name → username → bot_id.
func pickBotName(m *SlackMessage) string {
	if m.BotProfile != nil && m.BotProfile.Name != "" {
		return m.BotProfile.Name
	}
	if m.Username != "" {
		return m.Username
	}
	return m.BotID
}

// resolveAuthor returns the OneCamp UserInfo for the Slack message
// poster. Falls back to the importing admin if the Slack user isn't
// mapped (defensive — should only happen for bot messages whose user
// id differs from the bot's user id).
//
// Bot attribution: messages with only a bot_id and no `user` are
// attributed to the importing admin in the Postgres/Dgraph `created_by`
// edge, with the bot's name surfaced via a "[bot: <name>]" prefix in
// the rendered HTML body (see renderMessageHTML.isBotMessage). This
// keeps the schema simple — we don't need a synthetic user per bot —
// while preserving the visual distinction in the timeline.
func (mc *messageContext) resolveAuthor(ctx context.Context, m *SlackMessage, importingUser *userModels.UserInfo) (*userModels.UserInfo, error) {
	authorId := m.User
	if authorId == "" && m.BotID != "" {
		// Bot message with no human user id. Attribute to importing user;
		// the bot's name appears in the rendered body via the prefix
		// chip injected in renderMessageHTML.
		return importingUser, nil
	}
	if authorId == "" {
		return importingUser, nil
	}

	if cached, ok := mc.userCache[authorId]; ok {
		return cached, nil
	}

	ocUUID, err := importModels.LookupIdMapping(ctx, mc.importId, importModels.EntityUser, authorId)
	if err != nil {
		return importingUser, nil
	}
	if ocUUID == uuid.Nil {
		return importingUser, nil
	}

	pgUser, err := userBusiness.GetUserByUUID(ctx, ocUUID)
	if err != nil || pgUser == nil {
		return importingUser, nil
	}
	dgUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID.String())
	if err != nil || dgUser == nil {
		return importingUser, nil
	}

	info := &userModels.UserInfo{
		UserPostgresInfo: *pgUser,
		UserDgraphInfo:   *dgUser,
	}
	mc.userCache[authorId] = info
	return info, nil
}

// newMessageContext fetches channel info and primes caches.
func newMessageContext(ctx context.Context, chunk *importModels.Chunk, workspaceName string) (*messageContext, error) {
	if chunk.ChannelSlackId == nil {
		return nil, fmt.Errorf("missing channel_slack_id")
	}

	channelUUID, err := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityChannel, *chunk.ChannelSlackId)
	if err != nil || channelUUID == uuid.Nil {
		return nil, fmt.Errorf("channel not mapped: %s", *chunk.ChannelSlackId)
	}

	dgraphChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, "")
	if err != nil || dgraphChannel == nil {
		return nil, fmt.Errorf("get dgraph channel: %w", err)
	}

	return &messageContext{
		importId:       chunk.ImportId,
		chunkId:        chunk.Id,
		chunkType:      chunk.ChunkType,
		workspaceName:  workspaceName,
		channelSlackId: *chunk.ChannelSlackId,
		channelUUID:    channelUUID,
		dgraphChannel:  dgraphChannel,
		channelName:    dgraphChannel.Name,
		userCache:      make(map[string]*userModels.UserInfo, 32),
		userNameCache:  make(map[string]string, 32),
		channelCache:   make(map[string]channelRef, 8),
		enqueuedFiles:  make(map[string]struct{}, 16),
	}, nil
}

// flushMessageContext persists final metrics. Always called.
func flushMessageContext(ctx context.Context, mc *messageContext) {
	if mc == nil {
		return
	}
	// Drain any buffered file chunks. We deliberately do this before the
	// heartbeat so the file stage sees the chunks as soon as the message
	// chunk lands its FinishChunk, not on the next claim cycle.
	if len(mc.pendingFileChunks) > 0 {
		flushFileChunks(ctx, mc.pendingFileChunks)
		mc.pendingFileChunks = mc.pendingFileChunks[:0]
	}
	cursor := mc.lastCursor
	_ = importModels.HeartbeatChunk(ctx, mc.chunkId, mc.itemsDone, &cursor)
}

// buildFileChunk constructs a single ChunkFile row in memory without
// touching Postgres. Used by callers that buffer up file enqueues and
// flush in batch via flushFileChunks.
//
// Returns nil for files without a downloadable URL (private exports
// occasionally include zero-byte tombstones with empty url_private*).
//
// Encoding: the chunk's object_key column carries a small JSON object
// with the four fields the file worker needs (file id, url, name, size,
// parent ts). JSON tolerates Slack's URL query-strings and arbitrary
// filenames cleanly — earlier pipe-delimited encoding silently
// corrupted any filename or URL that contained a pipe or colon.
func buildFileChunk(importId uuid.UUID, channelSlackId, parentTs string, f SlackFile) *importModels.Chunk {
	url := f.URLPrivateDownload
	if url == "" {
		url = f.URLPrivate
	}

	if url == "" {
		return nil
	}

	meta := fileChunkMeta{
		FileID:   f.ID,
		URL:      url,
		Name:     helpers.SanitizeUploadFileName(f.Name),
		Size:     f.Size,
		ParentTs: parentTs,
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		// Marshalling a struct of basic types should never fail; if it
		// somehow does we'd rather skip enqueueing than panic.
		return nil
	}
	objectKey := string(encoded)

	return &importModels.Chunk{
		Id:             uuid.New(),
		ImportId:       importId,
		ChunkType:      importModels.ChunkFile,
		ChannelSlackId: ptrStr(channelSlackId),
		ObjectKey:      &objectKey,
		Status:         importModels.ChunkStatusPending,
		MaxAttempts:    5,
	}
}

// flushFileChunks batch-inserts a slice of pending file chunks. Caller
// is expected to clear its buffer after this returns. Errors are logged
// (not propagated) — a failed batch is recoverable on chunk replay
// because messages remain mapped and the file enqueue path runs before
// the idempotent message-skip on the next attempt.
//
// Uses a detached background context with a hard timeout so a parent
// ctx cancellation (operator hit Cancel) doesn't drop the buffered
// inserts the worker has already promised to enqueue. Without this,
// the queued files for messages we already mapped would be silently
// lost on cancellation.
func flushFileChunks(ctx context.Context, pending []*importModels.Chunk) {
	if len(pending) == 0 {
		return
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := importModels.CreateChunks(flushCtx, pending); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport.flushFileChunks insert failed batch=%d err=%+v", len(pending), err)
	}
}

// tsLE compares two Slack ts strings numerically. Slack ts is a fixed
// "<seconds>.<microseconds>" format so byte comparison after zero-padding
// agrees with numeric comparison; we do a direct lex compare which is
// equivalent because both sides are fixed-width.
func tsLE(a, b string) bool {
	if len(a) == len(b) {
		return a <= b
	}
	// Different widths: pad the shorter one. In practice every Slack ts
	// has the same width (10 + 1 + 6 = 17 chars).
	if len(a) < len(b) {
		return strings.Repeat("0", len(b)-len(a))+a <= b
	}
	return a <= strings.Repeat("0", len(a)-len(b))+b
}

// slackTsToTime parses "1705320000.123456" into time.Time.
func slackTsToTime(ts string) time.Time {
	dot := strings.IndexByte(ts, '.')
	if dot < 0 {
		return time.Now()
	}
	secs := ts[:dot]
	usecs := ts[dot+1:]
	var sec int64
	for _, c := range secs {
		if c < '0' || c > '9' {
			return time.Now()
		}
		sec = sec*10 + int64(c-'0')
	}
	var usec int64
	for _, c := range usecs {
		if c < '0' || c > '9' {
			break
		}
		usec = usec*10 + int64(c-'0')
	}
	return time.Unix(sec, usec*1000)
}

// backdatePost rewrites the created_at on a posts row so the imported
// content is timestamped correctly. The Postgres INSERT in
// domain.CreatePost uses DEFAULT NOW(); Dgraph + OpenSearch are already
// stamped via WithImportTimestamp in the calling context, so this
// UPDATE is what aligns the relational store with the rest. Idempotent
// — re-running pins to the same value.
func backdatePost(ctx context.Context, postUUID uuid.UUID, t time.Time) error {
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := importModels.Exec(dbCtx, `
		UPDATE posts SET created_at = $2, updated_at = $2 WHERE id = $1`, postUUID, t)
	return err
}

func backdateComment(ctx context.Context, commentUUID uuid.UUID, t time.Time) error {
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := importModels.Exec(dbCtx, `
		UPDATE comments SET created_at = $2, updated_at = $2 WHERE id = $1`, commentUUID, t)
	return err
}

// applyMessageEdit updates an already-imported post or comment with
// the latest text. Called when a Slack export contains a
// `message_changed` record for a ts we previously imported.
//
// Resolution order:
//  1. Look up the original ts in the per-import id_map (this run).
//  2. Fall back to the workspace map (prior run of this workspace).
//  3. Skip silently if neither has it — the edit refers to a message we
//     never imported (system message that was filtered, or a re-import
//     where the original was rolled back).
//
// Updates Postgres, Dgraph, and OpenSearch in lockstep via the canonical
// UpdatePost / UpdatePostComment business helpers. Those helpers honour
// the bulk-import context flag so MQTT/AI/webhook side-effects are
// suppressed — no live "post edited" notifications will fire for
// thousands of historical edits.
func applyMessageEdit(ctx context.Context, mc *messageContext, sub *SlackEditedMessage) error {
	originalTs := sub.Ts
	isComment := sub.ThreadTs != "" && sub.ThreadTs != sub.Ts

	entityType := importModels.EntityMessage
	if isComment {
		entityType = importModels.EntityComment
	}

	ocUUID, _ := importModels.LookupIdMapping(ctx, mc.importId, entityType, originalTs)
	if ocUUID == uuid.Nil && mc.workspaceName != "" {
		ocUUID, _ = importModels.LookupWorkspaceMapping(ctx, mc.workspaceName, entityType, originalTs)
	}
	if ocUUID == uuid.Nil {
		return nil
	}

	// Re-render with the new text + the same resolvers used at create time.
	pseudoMsg := &SlackMessage{
		Ts:       originalTs,
		User:     sub.User,
		Text:     sub.Text,
		Blocks:   sub.Blocks,
		ThreadTs: sub.ThreadTs,
	}
	html := renderMessageHTML(ctx, mc, pseudoMsg)

	// editedAt — Slack stores in the optional `edited.ts`. Fall back to
	// the message's own ts if absent.
	editedAt := slackTsToTime(originalTs)
	if sub.Edited != nil && sub.Edited.Ts != "" {
		editedAt = slackTsToTime(sub.Edited.Ts)
	}

	// Backdate the canonical update so updated_at lands on the Slack
	// edit time, not the import time.
	ctxBackdated := helpers.WithImportTimestamp(ctx, editedAt)

	if isComment {
		dgraphComment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctxBackdated, ocUUID.String(), "")
		if err != nil || dgraphComment == nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport applyMessageEdit: comment %s not found in Dgraph; PG-only fallback", ocUUID)
			_, _ = importModels.Exec(ctxBackdated, `
				UPDATE comments SET updated_at = $2 WHERE id = $1`, ocUUID, editedAt)
			return nil
		}
		commentInfo := &postAdapter.InputCreateOrUpdateCommentToPost{
			HTMLText: html,
			Uuid:     ocUUID.String(),
			PostUuid: dgraphComment.Post.Uuid,
		}
		if err := postBusiness.UpdatePostComment(ctxBackdated, ocUUID, commentInfo, nil, dgraphComment); err != nil {
			return fmt.Errorf("UpdatePostComment: %w", err)
		}
		return nil
	}

	// Channel post edit. Hydrate the basic Dgraph post info to learn
	// the channel uuid + author uuid the canonical UpdatePost path needs.
	dgraphPost, err := postBusiness.GetUnDeletedDgraphPostChannelBasicInfo(ctxBackdated, ocUUID.String())
	if err != nil || dgraphPost == nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport applyMessageEdit: post %s not found in Dgraph; PG-only fallback", ocUUID)
		_, _ = importModels.Exec(ctxBackdated, `
			UPDATE posts SET updated_at = $2 WHERE id = $1`, ocUUID, editedAt)
		return nil
	}
	channelId := ""
	if dgraphPost.Channel != nil {
		channelId = dgraphPost.Channel.Uuid
	}
	postByUUID := ""
	if dgraphPost.PostBy != nil {
		postByUUID = dgraphPost.PostBy.Uuid
	}
	postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    html,
		ChannelUuid: channelId,
	}
	if err := postBusiness.UpdatePost(ctxBackdated, postInfo, nil, ocUUID, channelId, postByUUID); err != nil {
		return fmt.Errorf("UpdatePost: %w", err)
	}
	return nil
}

// applyMessageDelete tombstones a previously-imported post/comment
// using the canonical DeletePost / DeleteCommentOnPost paths so all
// stores (Postgres + Dgraph + OpenSearch) and the cascading attachment/
// AI cleanups stay in lockstep with native deletes. The bulk-import
// gate inside those paths suppresses live MQTT and AI fan-out.
func applyMessageDelete(ctx context.Context, mc *messageContext, originalTs string) error {
	for _, et := range []string{importModels.EntityMessage, importModels.EntityComment} {
		ocUUID, _ := importModels.LookupIdMapping(ctx, mc.importId, et, originalTs)
		if ocUUID == uuid.Nil && mc.workspaceName != "" {
			ocUUID, _ = importModels.LookupWorkspaceMapping(ctx, mc.workspaceName, et, originalTs)
		}
		if ocUUID == uuid.Nil {
			continue
		}

		if et == importModels.EntityComment {
			dgraphComment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, ocUUID.String(), "")
			if err != nil || dgraphComment == nil {
				_, _ = importModels.Exec(ctx, `
					UPDATE comments SET deleted_at = NOW(), updated_at = NOW()
					WHERE id = $1 AND deleted_at IS NULL`, ocUUID)
				return nil
			}
			authorUUID := ""
			if dgraphComment.CommentBy != nil {
				authorUUID = dgraphComment.CommentBy.Uuid
			}
			if err := postBusiness.DeleteCommentOnPost(ctx, ocUUID, dgraphComment, authorUUID); err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport applyMessageDelete: DeleteCommentOnPost failed for %s: %+v", ocUUID, err)
				return err
			}
			helpers.LogInfoWithContext(ctx,
				"SlackImport tombstoned comment %s via canonical path", ocUUID)
			return nil
		}

		// Channel post delete.
		dgraphPost, err := postBusiness.GetUnDeletedDgraphPostChannelBasicInfo(ctx, ocUUID.String())
		if err != nil || dgraphPost == nil {
			_, _ = importModels.Exec(ctx, `
				UPDATE posts SET deleted_at = NOW(), updated_at = NOW()
				WHERE id = $1 AND deleted_at IS NULL`, ocUUID)
			return nil
		}
		channelId := ""
		if dgraphPost.Channel != nil {
			channelId = dgraphPost.Channel.Uuid
		}
		if err := postBusiness.DeletePost(ctx, dgraphPost, channelId); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport applyMessageDelete: DeletePost failed for %s: %+v", ocUUID, err)
			return err
		}
		helpers.LogInfoWithContext(ctx,
			"SlackImport tombstoned post %s via canonical path", ocUUID)
		return nil
	}
	return nil
}
