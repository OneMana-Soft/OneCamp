package business

import (
	"context"
	"fmt"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// processDMChunk imports DM and MPIM messages from a single per-day
// JSON file inside the staged Slack export. Mirrors processMessageChunk
// but writes to OneCamp's chats table via the existing CreateChat /
// CreateChatForGroup business paths.
//
// Idempotency, resume cursor, heartbeat: same shape as processMessageChunk.
//
// Threads in DMs/MPIMs: handled inline as a two-pass within the same
// daily file. Pass 1 imports top-level messages; pass 2 imports replies.
// The two-pass avoids the "reply before parent" race that would
// otherwise produce THREAD_PARENT_NOT_FOUND warnings on the few
// workspaces that thread within DMs.
//
// MQTT/AI/notification fan-out is suppressed via helpers.IsBulkImport
// (set by the orchestrator). The chat business layer honours this flag
// in both CreateChat and CreateChatForGroup.
func processDMChunk(ctx context.Context, arc *Archive, chunk *importModels.Chunk,
	skipSubtypes bool, importingUser *userModels.UserInfo, workspaceName string) error {

	if chunk.ObjectKey == nil || chunk.ChannelSlackId == nil {
		return fmt.Errorf("dm chunk missing object_key/slack_id")
	}

	dc, err := loadDMContext(ctx, chunk, workspaceName)
	if err != nil {
		return err
	}

	// Always drain the buffered file chunks on exit. Without this, an
	// early return from cancellation or a transient error would lose
	// the queued file work for messages we already mapped — those files
	// would never be re-enqueued because the messages are already in
	// id_map and the early-enqueue branch would skip them on retry.
	defer func() {
		if len(dc.pendingFileChunks) > 0 {
			flushFileChunks(ctx, dc.pendingFileChunks)
			dc.pendingFileChunks = dc.pendingFileChunks[:0]
		}
	}()

	resumeCursor := ""
	if chunk.LastCursor != nil {
		resumeCursor = *chunk.LastCursor
	}

	heartbeatEvery := 250
	committed := 0

	// Two-pass: pass 1 = top-level messages only; pass 2 = thread
	// replies only. The pass id distinguishes them.
	for pass := 1; pass <= 2; pass++ {
		passReplies := pass == 2

		// Re-open the zip reader for pass 2 since IterMessages consumes it.
		passZf, err := OpenInZip(arc.Reader, *chunk.ObjectKey)
		if err != nil {
			return err
		}

		err = IterMessages(ctx, arc, passZf, func(m *SlackMessage) bool {
			select {
			case <-ctx.Done():
				return false
			default:
			}

			if resumeCursor != "" && tsLE(m.Ts, resumeCursor) {
				return true
			}
			if skipSubtypes && SubtypeIsSystem(m.Subtype) {
				return true
			}
			if SubtypeIsTombstone(m.Subtype) {
				if pass == 1 && m.PreviousMessage != nil && m.PreviousMessage.Ts != "" {
					_ = applyDMDelete(ctx, dc, m.PreviousMessage.Ts)
				}
				return true
			}
			if m.Subtype == "message_changed" {
				if pass == 1 && m.SubMessage != nil && m.SubMessage.Ts != "" {
					_ = applyDMEdit(ctx, dc, m.SubMessage)
				}
				return true
			}

			isThreadReply := m.ThreadTs != "" && m.ThreadTs != m.Ts

			// Pass 1: top-level only. Pass 2: replies only.
			if isThreadReply != passReplies {
				return true
			}

			entityType := importModels.EntityMessage
			if isThreadReply {
				entityType = importModels.EntityComment
			}

			// Enqueue file chunks BEFORE the idempotency short-circuit
			// so a chunk that previously crashed after message creation
			// but before file scheduling still recovers on retry. We
			// only do it on pass 1 because pass 2 (thread replies) sees
			// the same files via the parent — duplicates would be
			// swallowed by the chunk unique index, but we save the
			// inserts entirely.
			if pass == 1 && len(m.Files) > 0 {
				for _, sf := range m.Files {
					if _, seen := dc.enqueuedFiles[sf.ID]; seen {
						continue
					}
					dc.enqueuedFiles[sf.ID] = struct{}{}
					if c := buildFileChunk(dc.importId, dc.slackId, m.Ts, sf); c != nil {
						dc.pendingFileChunks = append(dc.pendingFileChunks, c)
					}
				}
				if len(dc.pendingFileChunks) >= 200 {
					flushFileChunks(ctx, dc.pendingFileChunks)
					dc.pendingFileChunks = dc.pendingFileChunks[:0]
				}
			}

			if existing, _ := importModels.LookupIdMapping(ctx, dc.importId, entityType, m.Ts); existing != uuid.Nil {
				if !isThreadReply {
					dc.lastCursor = m.Ts
				}
				return true
			}

			if dc.workspaceName != "" {
				if existing, _ := importModels.LookupWorkspaceMapping(ctx, dc.workspaceName, entityType, m.Ts); existing != uuid.Nil {
					_ = importModels.UpsertIdMappingWithOwnership(ctx, dc.importId, entityType, m.Ts,
						existing, nil, mustMarshal(map[string]interface{}{"matched_by": "workspace_map"}),
						false)
					if !isThreadReply {
						dc.lastCursor = m.Ts
					}
					return true
				}
			}

			if isThreadReply {
				if err := importDMThreadReply(ctx, dc, m, importingUser); err != nil {
					importModels.LogImportError(ctx, dc.importId, &dc.chunkId,
						importModels.EntityComment, m.Ts,
						importModels.SeverityError, "DM_COMMENT_FAILED",
						err.Error(), nil)
					return true
				}
			} else {
				if err := importDMTopLevel(ctx, dc, m, importingUser); err != nil {
					importModels.LogImportError(ctx, dc.importId, &dc.chunkId,
						importModels.EntityMessage, m.Ts,
						importModels.SeverityError, "DM_MESSAGE_FAILED",
						err.Error(), nil)
					return true
				}
			}

			dc.itemsDone++
			if !isThreadReply {
				dc.lastCursor = m.Ts
			}
			committed++

			// Files are enqueued earlier in the loop (before the
			// idempotency short-circuit) so we don't duplicate that
			// work here.

			if committed%heartbeatEvery == 0 {
				cursor := dc.lastCursor
				_ = importModels.HeartbeatChunk(ctx, dc.chunkId, dc.itemsDone, &cursor)
			}
			return true
		})

		if err != nil {
			return err
		}
	}

	cursor := dc.lastCursor
	// pendingFileChunks are drained by the deferred flush at the top of
	// the function so a cancellation/error path doesn't lose the queue.
	return importModels.FinishChunk(ctx, dc.chunkId, dc.itemsDone, &cursor)
}

// importDMTopLevel imports one Slack DM/MPIM message as a OneCamp chat.
//
// 1:1 DMs use chatBusiness.CreateChat (takes a `sendToDgraph` param).
// MPIMs use chatBusiness.CreateChatForGroup (takes participant slice).
// We pick which by looking at dc.isMPIM.
func importDMTopLevel(ctx context.Context, dc *dmContext, m *SlackMessage, importingUser *userModels.UserInfo) error {
	authorInfo, _ := dc.resolveAuthor(ctx, m, importingUser)

	html := renderDMHTML(ctx, dc, m)
	if html == "" && len(m.Files) == 0 {
		return importModels.UpsertIdMapping(ctx, dc.importId, importModels.EntityMessage, m.Ts,
			uuid.New(), nil, mustMarshal(map[string]interface{}{"empty": true}))
	}

	chatInfo := &chatAdapter.ChatInfo{
		TextHtml: html,
		GrpUuid:  dc.groupingId,
	}

	if dc.isMPIM {
		// MPIMs: build the dgraph user list for participants.
		participants, err := mpimParticipantsDgraph(ctx, dc)
		if err != nil {
			return fmt.Errorf("mpim participants: %w", err)
		}
		chatInfo.Participants = dc.memberUUIDs

		// CreateChatForGroup interprets a non-empty GrpUuid as "join an
		// existing group" and SKIPS the Dgraph DM-node + participants
		// edge writes. We only want that behaviour after the first
		// imported message for this MPIM has materialised the DM node.
		// Tracking by id_map: if any prior message in this import
		// resolved to a chat under this grouping, we know the DM node
		// exists. The first message instead leaves GrpUuid empty so
		// CreateChatForGroup re-derives the same grouping ID via
		// GenerateGroupID(userUUIDs) and writes the DM node + edges.
		// Either path produces byte-identical GrpUuid, so downstream
		// reads match.
		if dc.dmNodeMaterialised {
			chatInfo.GrpUuid = dc.groupingId
		} else {
			chatInfo.GrpUuid = ""
		}

		// Backdate Dgraph + OpenSearch via context.
		slackTime := slackTsToTime(m.Ts)
		ctxBackdated := helpers.WithImportTimestamp(ctx, slackTime)
		created, err := chatBusiness.CreateChatForGroup(ctxBackdated, chatInfo, authorInfo, nil, participants)
		if err != nil {
			return fmt.Errorf("CreateChatForGroup: %w", err)
		}
		dc.dmNodeMaterialised = true
		chatUUID, err := uuid.Parse(created.Uuid)
		if err != nil {
			return fmt.Errorf("parse chat uuid: %w", err)
		}
		if err := backdateChat(ctx, chatUUID, slackTime); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport could not backdate chat %s: %+v", chatUUID, err)
		}
		if err := importModels.UpsertIdMappingWithOwnership(ctx, dc.importId,
			importModels.EntityMessage, m.Ts, chatUUID, nil,
			mustMarshal(map[string]interface{}{
				"grouping_id":    dc.groupingId,
				"author_slack":   m.User,
				"author_oc_uuid": authorInfo.UserPostgresInfo.Id.String(),
				"is_mpim":        true,
			}), true); err != nil {
			return err
		}
		if dc.workspaceName != "" {
			_ = importModels.UpsertWorkspaceMapping(ctx, dc.workspaceName,
				importModels.EntityMessage, m.Ts, chatUUID, dc.importId)
		}
		return nil
	}

	// 1:1 DM: pick the OTHER user as the recipient. dc.memberUUIDs has
	// exactly two entries for DMs.
	var recipientUUID uuid.UUID
	for _, u := range dc.memberUUIDs {
		if u != authorInfo.UserPostgresInfo.Id.String() {
			parsed, err := uuid.Parse(u)
			if err == nil {
				recipientUUID = parsed
				break
			}
		}
	}
	if recipientUUID == uuid.Nil {
		// Author isn't a member (defensive). Use the first non-author entry.
		for _, u := range dc.memberUUIDs {
			parsed, err := uuid.Parse(u)
			if err == nil && parsed != authorInfo.UserPostgresInfo.Id {
				recipientUUID = parsed
				break
			}
		}
	}
	if recipientUUID == uuid.Nil {
		return fmt.Errorf("dm has no resolvable recipient")
	}

	recipientDg := importingDgraphUser2(ctx, recipientUUID.String())
	if recipientDg == nil {
		return fmt.Errorf("dm recipient %s has no dgraph user", recipientUUID)
	}

	chatInfo.ToUuid = recipientUUID.String()
	// Backdate Dgraph + OpenSearch via context.
	slackTime := slackTsToTime(m.Ts)
	ctxBackdated := helpers.WithImportTimestamp(ctx, slackTime)
	created, err := chatBusiness.CreateChat(ctxBackdated, chatInfo, authorInfo, recipientDg, recipientUUID, nil)
	if err != nil {
		return fmt.Errorf("CreateChat: %w", err)
	}
	chatUUID, err := uuid.Parse(created.Uuid)
	if err != nil {
		return fmt.Errorf("parse chat uuid: %w", err)
	}
	if err := backdateChat(ctx, chatUUID, slackTime); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport could not backdate chat %s: %+v", chatUUID, err)
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, dc.importId,
		importModels.EntityMessage, m.Ts, chatUUID, nil,
		mustMarshal(map[string]interface{}{
			"grouping_id":    dc.groupingId,
			"author_slack":   m.User,
			"author_oc_uuid": authorInfo.UserPostgresInfo.Id.String(),
			"is_mpim":        false,
		}), true); err != nil {
		return err
	}
	if dc.workspaceName != "" {
		_ = importModels.UpsertWorkspaceMapping(ctx, dc.workspaceName,
			importModels.EntityMessage, m.Ts, chatUUID, dc.importId)
	}
	return nil
}

// importDMThreadReply attaches a Slack thread reply as a chat comment
// using the existing chatBusiness.CreateChatComment path. Channel
// thread replies follow a different path (importThreadReply in
// message_worker) because they live as Post comments, not chat comments.
func importDMThreadReply(ctx context.Context, dc *dmContext, m *SlackMessage, importingUser *userModels.UserInfo) error {
	parentUUID, _ := importModels.LookupIdMapping(ctx, dc.importId, importModels.EntityMessage, m.ThreadTs)
	if parentUUID == uuid.Nil {
		importModels.LogImportError(ctx, dc.importId, &dc.chunkId,
			importModels.EntityComment, m.Ts,
			importModels.SeverityWarning, "THREAD_PARENT_NOT_FOUND",
			"DM thread reply skipped because parent message was not imported",
			mustMarshal(map[string]interface{}{"thread_ts": m.ThreadTs}))
		return nil
	}

	authorInfo, _ := dc.resolveAuthor(ctx, m, importingUser)
	html := renderDMHTML(ctx, dc, m)
	if html == "" && len(m.Files) == 0 {
		return importModels.UpsertIdMapping(ctx, dc.importId, importModels.EntityComment, m.Ts,
			uuid.New(), &m.ThreadTs, mustMarshal(map[string]interface{}{"empty": true}))
	}

	// Need the parent dgraph chat.
	dgraphChat, err := chatBusiness.GetDgraphChatByUUID(ctx, parentUUID.String())
	if err != nil || dgraphChat == nil {
		return fmt.Errorf("get parent dgraph chat: %w", err)
	}

	commentInfo := &chatAdapter.InputCreateOrUpdateCommentToChat{
		HTMLText: html,
		ChatUuid: parentUUID.String(),
	}

	// Backdate Dgraph + OpenSearch via context.
	slackTime := slackTsToTime(m.Ts)
	ctxBackdated := helpers.WithImportTimestamp(ctx, slackTime)

	created, err := chatBusiness.CreateChatComment(ctxBackdated, commentInfo, authorInfo, nil,
		dc.groupingId, authorInfo.UserDgraphInfo.Uid, dgraphChat)
	if err != nil {
		return fmt.Errorf("CreateChatComment: %w", err)
	}
	commentUUID, err := uuid.Parse(created.Uuid)
	if err != nil {
		return importModels.UpsertIdMapping(ctx, dc.importId, importModels.EntityComment, m.Ts,
			uuid.New(), &m.ThreadTs, mustMarshal(map[string]interface{}{"unparsed_uuid": created.Uuid}))
	}
	if err := backdateComment(ctx, commentUUID, slackTime); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport could not backdate chat comment %s: %+v", commentUUID, err)
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, dc.importId,
		importModels.EntityComment, m.Ts, commentUUID, &m.ThreadTs,
		mustMarshal(map[string]interface{}{
			"author_slack": m.User,
			"thread_ts":    m.ThreadTs,
		}), true); err != nil {
		return err
	}
	if dc.workspaceName != "" {
		_ = importModels.UpsertWorkspaceMapping(ctx, dc.workspaceName,
			importModels.EntityComment, m.Ts, commentUUID, dc.importId)
	}
	return nil
}

// renderDMHTML mirrors renderMessageHTML for the DM context. The same
// resolvers are reused; the only difference is the parent context type.
//
// Bot attribution: same as the channel path — messages from bots get a
// "[bot: <name>]" prefix span so the imported timeline preserves the
// human/bot distinction. Slack apps DM users (think OAuth or workflow
// notifications) so this matters for chats too.
func renderDMHTML(ctx context.Context, dc *dmContext, m *SlackMessage) string {
	resolvers := mrkdwnResolvers{
		resolveUser: func(slackId string) (string, string, bool) {
			ocUUID, err := importModels.LookupIdMapping(ctx, dc.importId, importModels.EntityUser, slackId)
			if err != nil || ocUUID == uuid.Nil {
				return "", "", false
			}
			return ocUUID.String(), dc.userNameCache[slackId], true
		},
		// Channel mentions in DMs do happen (cross-link to a channel).
		// Resolve via the standard channel id_map and hydrate the human
		// name from id_map metadata so the rendered "#channel" stays
		// recognisable instead of falling back to a bare hash sign.
		resolveChannel: func(slackId string) (string, string, bool) {
			ocUUID, err := importModels.LookupIdMapping(ctx, dc.importId, importModels.EntityChannel, slackId)
			if err != nil || ocUUID == uuid.Nil {
				return "", "", false
			}
			name := ""
			if md, _ := importModels.GetIdMapMetadata(ctx, dc.importId, importModels.EntityChannel, slackId); len(md) > 0 {
				var meta struct {
					FinalName string `json:"final_name"`
				}
				_ = jsonUnmarshalRaw(md, &meta)
				name = meta.FinalName
			}
			return ocUUID.String(), name, true
		},
	}
	body := RenderSlackText(ctx, m, resolvers)

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

// lookupDgraphUser resolves a OneCamp UUID to the cached DgraphUser.
// Caller may need the full *dgraphStruct.DgraphUser for chat business
// calls; use importingDgraphUser2 directly for that path.
func (dc *dmContext) lookupDgraphUser(ctx context.Context, ocUUID string) *dgraphStruct.DgraphUser {
	if u, ok := dc.userCache[ocUUID]; ok {
		return &u.UserDgraphInfo
	}
	return importingDgraphUser2(ctx, ocUUID)
}

// mpimParticipantsDgraph builds the []*DgraphUser slice CreateChatForGroup
// expects. Each member's full Dgraph user is fetched once.
func mpimParticipantsDgraph(ctx context.Context, dc *dmContext) ([]*dgraphStruct.DgraphUser, error) {
	out := make([]*dgraphStruct.DgraphUser, 0, len(dc.memberUUIDs))
	for _, u := range dc.memberUUIDs {
		dgUser := importingDgraphUser2(ctx, u)
		if dgUser == nil {
			continue
		}
		out = append(out, dgUser)
	}
	return out, nil
}

// importingDgraphUser2 fetches a DgraphUser by OneCamp UUID. Returns
// nil when the user can't be found so the caller can skip that
// participant rather than failing the whole MPIM.
func importingDgraphUser2(ctx context.Context, ocUUID string) *dgraphStruct.DgraphUser {
	u, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID)
	if err != nil {
		return nil
	}
	return u
}

// applyDMEdit / applyDMDelete update an already-imported chat or
// chat-comment in lockstep across Postgres + Dgraph + OpenSearch via
// the canonical UpdateChat / UpdateChatComment / DeleteChat /
// DeleteCommentOnChat business paths. Those paths honour the
// bulk-import flag so MQTT/AI/webhook side-effects are suppressed.
//
// Lookup ladder: per-import id_map first, then workspace map. A miss
// (the edit/delete refers to a message we never imported) is silently
// skipped — most often this is a system message we filtered out.
func applyDMEdit(ctx context.Context, dc *dmContext, sub *SlackEditedMessage) error {
	originalTs := sub.Ts
	isComment := sub.ThreadTs != "" && sub.ThreadTs != sub.Ts
	entityType := importModels.EntityMessage
	if isComment {
		entityType = importModels.EntityComment
	}
	ocUUID, _ := importModels.LookupIdMapping(ctx, dc.importId, entityType, originalTs)
	if ocUUID == uuid.Nil && dc.workspaceName != "" {
		ocUUID, _ = importModels.LookupWorkspaceMapping(ctx, dc.workspaceName, entityType, originalTs)
	}
	if ocUUID == uuid.Nil {
		return nil
	}

	editedAt := slackTsToTime(originalTs)
	if sub.Edited != nil && sub.Edited.Ts != "" {
		editedAt = slackTsToTime(sub.Edited.Ts)
	}
	ctxBackdated := helpers.WithImportTimestamp(ctx, editedAt)

	// Re-render with the same resolvers used at create time.
	pseudoMsg := &SlackMessage{
		Ts:       originalTs,
		User:     sub.User,
		Text:     sub.Text,
		Blocks:   sub.Blocks,
		ThreadTs: sub.ThreadTs,
	}
	html := renderDMHTML(ctxBackdated, dc, pseudoMsg)

	if isComment {
		dgraphComment, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctxBackdated, ocUUID.String(), "")
		if err != nil || dgraphComment == nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport applyDMEdit: chat-comment %s not found in Dgraph; PG-only fallback", ocUUID)
			_, _ = importModels.Exec(ctxBackdated, `
				UPDATE comments SET updated_at = $2 WHERE id = $1`, ocUUID, editedAt)
			return nil
		}
		// rawDgraphChat for UpdateChatComment must carry Uuid + DM.GroupingId.
		rawChat := &dgraphStruct.DgraphChat{
			Uuid: dgraphComment.Chat.Uuid,
			DM:   &dgraphStruct.DgraphDm{GroupingId: dc.groupingId},
		}
		commentInfo := &chatAdapter.InputCreateOrUpdateCommentToChat{
			HTMLText: html,
			ChatUuid: dgraphComment.Chat.Uuid,
			Uuid:     ocUUID.String(),
		}
		if err := chatBusiness.UpdateChatComment(ctxBackdated, ocUUID, commentInfo, nil, rawChat, dgraphComment); err != nil {
			return fmt.Errorf("UpdateChatComment: %w", err)
		}
		return nil
	}

	// Top-level chat edit. We need the author UUID to drive Dgraph's
	// UpdateChat path. Look it up from the id_map metadata captured at
	// creation time so we don't re-query Dgraph here.
	authorUUID := ""
	if md, _ := importModels.GetIdMapMetadata(ctx, dc.importId, importModels.EntityMessage, originalTs); len(md) > 0 {
		var meta struct {
			AuthorOC string `json:"author_oc_uuid"`
		}
		_ = jsonUnmarshalRaw(md, &meta)
		authorUUID = meta.AuthorOC
	}

	chatInfo := &chatAdapter.ChatInfo{
		Uuid:     ocUUID.String(),
		TextHtml: html,
		GrpUuid:  dc.groupingId,
	}
	if err := chatBusiness.UpdateChat(ctxBackdated, chatInfo, ocUUID, dc.groupingId, authorUUID, nil); err != nil {
		return fmt.Errorf("UpdateChat: %w", err)
	}
	return nil
}

func applyDMDelete(ctx context.Context, dc *dmContext, originalTs string) error {
	for _, et := range []string{importModels.EntityMessage, importModels.EntityComment} {
		ocUUID, _ := importModels.LookupIdMapping(ctx, dc.importId, et, originalTs)
		if ocUUID == uuid.Nil && dc.workspaceName != "" {
			ocUUID, _ = importModels.LookupWorkspaceMapping(ctx, dc.workspaceName, et, originalTs)
		}
		if ocUUID == uuid.Nil {
			continue
		}

		if et == importModels.EntityComment {
			dgraphComment, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, ocUUID.String(), "")
			if err != nil || dgraphComment == nil {
				_, _ = importModels.Exec(ctx, `
					UPDATE comments SET deleted_at = NOW(), updated_at = NOW()
					WHERE id = $1 AND deleted_at IS NULL`, ocUUID)
				return nil
			}
			chatUuid := ""
			if dgraphComment.Chat != nil {
				chatUuid = dgraphComment.Chat.Uuid
			}
			authorUUID := ""
			if dgraphComment.CommentBy != nil {
				authorUUID = dgraphComment.CommentBy.Uuid
			}
			if err := chatBusiness.DeleteCommentOnChat(ctx, ocUUID, authorUUID, chatUuid, dgraphComment); err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport applyDMDelete: DeleteCommentOnChat failed for %s: %+v", ocUUID, err)
				return err
			}
			helpers.LogInfoWithContext(ctx,
				"SlackImport tombstoned chat-comment %s via canonical path", ocUUID)
			return nil
		}

		// Top-level chat delete.
		dgraphChat, err := chatBusiness.GetDgraphChatByUUID(ctx, ocUUID.String())
		if err != nil || dgraphChat == nil {
			_, _ = importModels.Exec(ctx, `
				UPDATE chats SET deleted_at = NOW(), updated_at = NOW()
				WHERE id = $1 AND deleted_at IS NULL`, ocUUID)
			return nil
		}
		// DeleteChat needs DM.GroupingId on rawDgraphChat for the MQTT
		// delete fan-out path; the canonical fetch above doesn't fill
		// it in, so we attach the precomputed grouping id from dc.
		if dgraphChat.DM == nil {
			dgraphChat.DM = &dgraphStruct.DgraphDm{}
		}
		if dgraphChat.DM.GroupingId == "" {
			dgraphChat.DM.GroupingId = dc.groupingId
		}
		chatInfo := &chatAdapter.ChatInfo{Uuid: ocUUID.String(), GrpUuid: dc.groupingId}
		if err := chatBusiness.DeleteChat(ctx, chatInfo, ocUUID, dc.groupingId, dgraphChat); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport applyDMDelete: DeleteChat failed for %s: %+v", ocUUID, err)
			return err
		}
		helpers.LogInfoWithContext(ctx,
			"SlackImport tombstoned chat %s via canonical path", ocUUID)
		return nil
	}
	return nil
}

// backdateChat is the chat-table equivalent of backdatePost.
func backdateChat(ctx context.Context, chatUUID uuid.UUID, t time.Time) error {
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := importModels.Exec(dbCtx, `
		UPDATE chats SET created_at = $2, updated_at = $2 WHERE id = $1`, chatUUID, t)
	return err
}
