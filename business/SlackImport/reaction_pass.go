package business

import (
	"context"
	"sync"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// processReactionPass walks every per-day file again, extracting the
// `reactions` array from each message and applying them to the imported
// posts/comments via the existing CreateOrUpdatePostReaction (and
// CreateOrUpdatePostCommentReaction for thread replies) business paths.
//
// The pass runs single-threaded (one chunk) on purpose: reaction writes
// hit the same Dgraph nodes a message worker just touched, and Dgraph
// upsert ordering is sensitive to concurrent writes against the same
// uid. Sequential is also plenty fast — even 100k reactions complete in
// minutes.
//
// MQTT/notification side-effects are suppressed via the bulk-import
// context flag (see helpers.IsBulkImport) so 50k historical reactions
// don't spam every channel member.
//
// Resolution rules:
//  1. Slack emoji name → OneCamp emoji_id via slack_emoji_map. Misses
//     become CUSTOM_EMOJI_UNSUPPORTED warnings.
//  2. Reactor → resolved Dgraph user. Workspace-map miss = skip.
//  3. Parent message/comment → already imported. Map miss = skip
//     (parent never imported, e.g. system message that was filtered).
//
// Comment reactions: reactions on Slack thread replies are imported as
// reactions on the corresponding OneCamp comment via the channel-post
// CreateOrUpdatePostCommentReaction path. DM/MPIM thread reply
// reactions are out of scope for this pass — those are handled by a
// V2 chat-comment-reaction extension once the chat-side comment table
// is ready.
func processReactionPass(ctx context.Context, arc *Archive, chunk *importModels.Chunk,
	parsed *ParsedExport) error {

	rs := newReactionState(chunk.ImportId)
	applied := 0
	skippedEmoji := 0
	skippedUser := 0
	skippedComment := 0

	for slackChannelId, dailyFiles := range parsed.MessageFiles {
		// V1: import reactions for everything we wrote in earlier
		// stages — public channels and private groups. DMs/MPIMs are
		// handled by their own pipeline.
		if !isImportableChannelId(parsed, slackChannelId) {
			continue
		}

		// Resolve channel info once per channel.
		channelUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
			importModels.EntityChannel, slackChannelId)
		if channelUUID == uuid.Nil {
			continue
		}

		for _, dailyPath := range dailyFiles {
			zf, err := OpenInZip(arc.Reader, dailyPath)
			if err != nil {
				continue
			}
			err = IterMessages(ctx, arc, zf, func(m *SlackMessage) bool {
				select {
				case <-ctx.Done():
					return false
				default:
				}
				if len(m.Reactions) == 0 {
					return true
				}

				// Resolve which entity the reactions go on.
				isComment := m.ThreadTs != "" && m.ThreadTs != m.Ts
				if isComment {
					n, err := applyCommentReactions(ctx, chunk, parsed, slackChannelId, m, rs)
					if err != nil {
						helpers.LogWarnWithContext(ctx,
							"SlackImport comment-reaction apply failed ts=%s err=%+v", m.Ts, err)
					}
					applied += n.applied
					skippedEmoji += n.skippedEmoji
					skippedUser += n.skippedUser
					skippedComment += n.skippedComment
					return true
				}

				postUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
					importModels.EntityMessage, m.Ts)
				if postUUID == uuid.Nil {
					return true
				}

				// Author dgraph uid for ownership disambiguation.
				postOwnerUid, postOwnerUuid := rs.lookupAuthor(ctx, m.User)

				for _, r := range m.Reactions {
					emojiId, err := importModels.LookupSlackEmoji(ctx, r.Name)
					if err != nil {
						helpers.LogWarnWithContext(ctx,
							"SlackImport reaction emoji lookup failed name=%s err=%+v",
							r.Name, err)
						continue
					}
					if emojiId == "" {
						skippedEmoji++
						// Single warning per (message, emoji) so we
						// don't double-up if the operator re-runs.
						importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
							importModels.EntityMessage, m.Ts,
							importModels.SeverityWarning,
							"CUSTOM_EMOJI_UNSUPPORTED",
							"emoji "+r.Name+" not in slack_emoji_map; reaction skipped",
							mustMarshal(map[string]interface{}{"emoji": r.Name}))
						continue
					}

					for _, slackUserId := range r.Users {
						reactorUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
							importModels.EntityUser, slackUserId)
						if reactorUUID == uuid.Nil {
							skippedUser++
							continue
						}
						reactorDgUid, _ := rs.lookupUser(ctx, reactorUUID.String())
						if reactorDgUid == "" {
							skippedUser++
							continue
						}

						// Each reactor gets one row. CreateOrUpdatePostReaction
						// is upsert-shaped; calling twice for the same
						// (post, user) is a no-op.
						_, err := postBusiness.CreateOrUpdatePostReaction(ctx,
							&postAdapter.InputUpdateReactionForPost{
								EmojiUuid: emojiId,
								Uuid:      postUUID.String(),
							},
							postOwnerUid,
							postOwnerUuid,
							reactorDgUid,
							reactorUUID.String(),
							rs.userName(reactorUUID.String()),
							channelUUID.String(),
						)
						if err != nil {
							helpers.LogWarnWithContext(ctx,
								"SlackImport reaction write failed post=%s emoji=%s user=%s err=%+v",
								postUUID, emojiId, reactorUUID, err)
							continue
						}
						applied++
					}
				}
				return true
			})
			if err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport reaction pass failed on %s: %+v", dailyPath, err)
			}
		}
	}

	patch := mustMarshal(map[string]interface{}{
		"reactions": map[string]int{
			"applied":         applied,
			"skipped_emoji":   skippedEmoji,
			"skipped_user":    skippedUser,
			"skipped_comment": skippedComment,
		},
	})
	_ = importModels.UpdateProgress(ctx, chunk.ImportId, patch)
	return importModels.FinishChunk(ctx, chunk.Id, applied, nil)
}

// reactionTally is a small struct used to bubble per-message counts
// back to the outer loop without juggling four return values.
type reactionTally struct {
	applied        int
	skippedEmoji   int
	skippedUser    int
	skippedComment int
}

// applyCommentReactions applies every reaction on a Slack thread reply
// (which we imported as a OneCamp post comment) to its corresponding
// OneCamp comment.
//
// The reaction pass only ever sees this for channel-like conversations
// because the outer loop filters out DM/MPIM channels via
// isImportableChannelId before reaching this code. Channel thread
// replies always live as Post comments in OneCamp, so we use the
// post-comment reaction path here.
//
// Resolution mirrors the post-reaction path: emoji name → emoji_id,
// reactor → DgraphUser. The expensive piece — fetching the comment's
// Dgraph node — is done once per message because a single comment
// typically carries several reactions.
func applyCommentReactions(ctx context.Context, chunk *importModels.Chunk,
	_ *ParsedExport, _ string, m *SlackMessage, rs *reactionState) (reactionTally, error) {

	tally := reactionTally{}

	commentUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
		importModels.EntityComment, m.Ts)
	if commentUUID == uuid.Nil {
		// Parent comment was filtered out (e.g., empty body + no files).
		// Slack still ships reactions for it; nothing to attach to.
		tally.skippedComment += len(m.Reactions)
		return tally, nil
	}

	// Resolve a Dgraph view of the comment so the reaction business
	// path can stitch the edge. The id_map metadata blob carries the
	// author's Slack user id; we use the comment-by-uuid Dgraph fetch
	// because the post-comment-reaction call needs Post.Channel.Uuid.
	dgraphComment, err := commentBusinessGetByUUID(ctx, commentUUID.String())
	if err != nil || dgraphComment == nil {
		tally.skippedComment += len(m.Reactions)
		return tally, err
	}

	for _, r := range m.Reactions {
		emojiId, err := importModels.LookupSlackEmoji(ctx, r.Name)
		if err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport comment-reaction emoji lookup failed name=%s err=%+v",
				r.Name, err)
			continue
		}
		if emojiId == "" {
			tally.skippedEmoji++
			importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
				importModels.EntityComment, m.Ts,
				importModels.SeverityWarning,
				"CUSTOM_EMOJI_UNSUPPORTED",
				"emoji "+r.Name+" not in slack_emoji_map; comment reaction skipped",
				mustMarshal(map[string]interface{}{"emoji": r.Name}))
			continue
		}

		for _, slackUserId := range r.Users {
			reactorUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
				importModels.EntityUser, slackUserId)
			if reactorUUID == uuid.Nil {
				tally.skippedUser++
				continue
			}
			// Hydrate the reactor's full Dgraph user — the reaction
			// path reads .Uid and .UserName.
			reactorDg := rs.lookupFullUser(ctx, reactorUUID.String())
			if reactorDg == nil || reactorDg.Uid == "" {
				tally.skippedUser++
				continue
			}

			_, err := postBusiness.CreateOrUpdatePostCommentReaction(ctx,
				&postAdapter.InputUpdateReactionForCommentInPost{
					EmojiUuid: emojiId,
					Uuid:      commentUUID.String(),
				},
				dgraphComment,
				reactorDg,
			)
			if err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport comment-reaction write failed comment=%s emoji=%s user=%s err=%+v",
					commentUUID, emojiId, reactorUUID, err)
				continue
			}
			tally.applied++
		}
	}
	return tally, nil
}

// commentBusinessGetByUUID is a thin wrapper around the Comment domain
// fetch. We pass an empty userDgraphUID because the Slack reaction
// pass operates with admin authority — the membership/admin counters
// returned by the underlying query are unused, so the empty uid just
// means "0/0", which is fine for our purposes.
func commentBusinessGetByUUID(ctx context.Context, commentUUID string) (*dgraphStruct.DgraphComment, error) {
	return commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, commentUUID, "")
}

// reactionState caches per-user Dgraph info across a single reaction
// pass. We expect heavy reuse: every reactor in a busy workspace shows
// up in dozens or hundreds of reactions.
type reactionState struct {
	importId uuid.UUID

	mu          sync.Mutex
	dgUsers     map[string]*dgraphStruct.DgraphUser // ocUUID -> dgraph user
	authors     map[string]string                   // slackUserId -> dgraph uid
	authorUUIDs map[string]string                   // slackUserId -> ocUUID
}

func newReactionState(importId uuid.UUID) *reactionState {
	return &reactionState{
		importId:    importId,
		dgUsers:     make(map[string]*dgraphStruct.DgraphUser, 64),
		authors:     make(map[string]string, 64),
		authorUUIDs: make(map[string]string, 64),
	}
}

// lookupUser resolves a OneCamp user UUID to its Dgraph uid + display name.
// Returns ("", "") on miss.
func (s *reactionState) lookupUser(ctx context.Context, ocUUID string) (string, string) {
	u := s.lookupFullUser(ctx, ocUUID)
	if u == nil {
		return "", ""
	}
	return u.Uid, u.UserName
}

// lookupFullUser is the same as lookupUser but returns the full
// DgraphUser. Used by paths that need ProfileKey / IsExternal in
// addition to the uid.
func (s *reactionState) lookupFullUser(ctx context.Context, ocUUID string) *dgraphStruct.DgraphUser {
	s.mu.Lock()
	if u, ok := s.dgUsers[ocUUID]; ok {
		s.mu.Unlock()
		return u
	}
	s.mu.Unlock()

	u, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID)
	if err != nil || u == nil {
		return nil
	}
	s.mu.Lock()
	s.dgUsers[ocUUID] = u
	s.mu.Unlock()
	return u
}

// userName retrieves a cached display name. Falls back to empty string
// if the user wasn't pre-resolved (caller should call lookupUser first).
func (s *reactionState) userName(ocUUID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.dgUsers[ocUUID]; ok {
		return u.UserName
	}
	return ""
}

// lookupAuthor returns (dgraphUid, ocUUID) for the original message
// poster. Used to set postOwnerUUID on reaction writes so the Dgraph
// activity row is correct.
func (s *reactionState) lookupAuthor(ctx context.Context, slackUserId string) (string, string) {
	if slackUserId == "" {
		return "", ""
	}
	s.mu.Lock()
	if uid, ok := s.authors[slackUserId]; ok {
		ocUUID := s.authorUUIDs[slackUserId]
		s.mu.Unlock()
		return uid, ocUUID
	}
	s.mu.Unlock()

	authorUUID, _ := importModels.LookupIdMapping(ctx, s.importId, importModels.EntityUser, slackUserId)
	if authorUUID == uuid.Nil {
		return "", ""
	}
	uid, _ := s.lookupUser(ctx, authorUUID.String())
	s.mu.Lock()
	s.authors[slackUserId] = uid
	s.authorUUIDs[slackUserId] = authorUUID.String()
	s.mu.Unlock()
	return uid, authorUUID.String()
}
