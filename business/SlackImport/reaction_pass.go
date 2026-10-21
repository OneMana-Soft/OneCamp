package business

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// processReactionPass walks every per-day file again and brings each
// message's reactions across onto what the message became: a channel's post
// or reply, or a DM's or group's message or reply (reactionWriter), through
// business/Reaction's one rule (one emoji per person, each once). Then the
// channels' pins (pin_pass.go).
//
// One chunk, on purpose: reaction writes hit the nodes the message workers
// just wrote, and the graph's upserts are sensitive to concurrent writes on
// one node. MQTT and notices are off (helpers.IsBulkImport): history tells
// nobody.
//
// A Slack emoji name becomes a OneCamp emoji by slack_emoji_map (a miss is
// noted, CUSTOM_EMOJI_UNSUPPORTED), a reactor the person they came across as
// (a miss is skipped), and a message what it became (a miss is a message that
// wasn't brought across, such as a filtered system message). A write that
// fails is noted (REACTION_IMPORT_FAILED): it used to be only logged.
func processReactionPass(ctx context.Context, arc *Archive, chunk *importModels.Chunk,
	parsed *ParsedExport) error {

	rs := newReactionState(chunk.ImportId)
	conversations := rs.conversations(ctx, parsed)
	var total reactionTally
	var notes []importModels.ImportError
	for slackChannelId, dailyFiles := range parsed.MessageFiles {
		conv, ok := conversations[slackChannelId]
		if !ok {
			continue
		}
		for _, dailyPath := range dailyFiles {
			zf, err := OpenInZip(arc.Reader, dailyPath)
			if err != nil {
				continue
			}
			// A batch's messages and replies are read at once, as are
			// emoji and people (once a pass).
			err = iterBatches(ctx, arc, zf, "", func(batch []*SlackMessage) {
				rs.prefetch(ctx, batch)
			}, func(m *SlackMessage) bool {
				if ctx.Err() != nil {
					return false
				}
				if len(m.Reactions) == 0 {
					return true
				}
				put, ok := rs.writerFor(ctx, conv, m)
				if !ok {
					total.skippedComment += len(m.Reactions)
					return true
				}
				tally, noted := rs.applyReactions(ctx, m, put)
				total.add(tally)
				notes = append(notes, noted...)
				return true
			})
			if err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport reaction pass failed on %s: %+v", dailyPath, err)
			}
		}
	}
	importModels.LogImportErrors(ctx, chunk.ImportId, &chunk.Id, notes)

	// The channels' pins, now that their messages are here (pin_pass.go).
	pinned, pinSkipped := applyPins(ctx, chunk.ImportId, chunk.Id, parsed)

	patch := mustMarshal(map[string]interface{}{
		"reactions": map[string]int{
			"applied":         total.applied,
			"skipped_emoji":   total.skippedEmoji,
			"skipped_user":    total.skippedUser,
			"skipped_comment": total.skippedComment,
			"failed":          total.failed,
		},
		"pins": map[string]int{"applied": pinned, "skipped": pinSkipped},
	})
	_ = importModels.UpdateProgress(ctx, chunk.ImportId, patch)
	return importModels.FinishChunk(ctx, chunk.Id, total.applied, nil)
}

// reactionTally is what a message's reactions came to.
type reactionTally struct {
	applied        int
	skippedEmoji   int
	skippedUser    int
	skippedComment int
	failed         int
}

func (t *reactionTally) add(o reactionTally) {
	t.applied += o.applied
	t.skippedEmoji += o.skippedEmoji
	t.skippedUser += o.skippedUser
	t.skippedComment += o.skippedComment
	t.failed += o.failed
}

// reactionConversation is a Slack conversation as the import brought it
// across: a channel (by its uuid) or a DM or group (by its grouping id).
type reactionConversation struct {
	channel uuid.UUID
	grp     string
}

// conversations is each of the export's conversations the import brought
// across, by Slack id: its channels' mappings and its DMs' and groups'
// grouping ids, read in at most three statements.
func (s *reactionState) conversations(ctx context.Context, parsed *ParsedExport) map[string]reactionConversation {
	out := map[string]reactionConversation{}
	var channels, dms, mpims []string
	for id := range parsed.MessageFiles {
		switch {
		case isImportableChannelId(parsed, id):
			channels = append(channels, id)
		case isDMId(parsed, id):
			dms = append(dms, id)
		case isMPIMId(parsed, id):
			mpims = append(mpims, id)
		}
	}
	if found, err := importModels.LookupIdMappingsMany(ctx, s.importId, importModels.EntityChannel, channels); err == nil {
		for id, u := range found {
			if u != uuid.Nil {
				out[id] = reactionConversation{channel: u}
			}
		}
	}
	grouping := func(entity string, ids []string) {
		meta, err := importModels.IdMapMetadataMany(ctx, s.importId, entity, ids)
		if err != nil {
			return
		}
		for id, raw := range meta {
			var m struct {
				GroupingID string `json:"grouping_id"`
			}
			if json.Unmarshal(raw, &m) == nil && m.GroupingID != "" {
				out[id] = reactionConversation{grp: m.GroupingID}
			}
		}
	}
	grouping(importModels.EntityDM, dms)
	grouping(importModels.EntityMPIM, mpims)
	return out
}

// reactionWriter writes one person's reaction (a OneCamp emoji) on what a
// message became.
type reactionWriter func(emoji string, reactor *dgraphStruct.DgraphUser) error

// writerFor is the writer for m's reactions in conv: on the post, reply,
// message or reply m became; ok false when m wasn't brought across.
func (s *reactionState) writerFor(ctx context.Context, conv reactionConversation, m *SlackMessage) (reactionWriter, bool) {
	reply := m.ThreadTs != "" && m.ThreadTs != m.Ts
	entity := importModels.EntityMessage
	if reply {
		entity = importModels.EntityComment
	}
	target := s.mapping(ctx, entity, m.Ts)
	if target == uuid.Nil {
		return nil, false
	}
	id := target.String()
	switch {
	case conv.channel != uuid.Nil && !reply:
		ownerUid, ownerUuid := s.lookupAuthor(ctx, m.User)
		return func(emoji string, reactor *dgraphStruct.DgraphUser) error {
			_, err := postBusiness.CreateOrUpdatePostReaction(ctx, &postAdapter.InputUpdateReactionForPost{EmojiUuid: emoji, Uuid: id},
				ownerUid, ownerUuid, reactor.Uid, reactor.Uuid, reactor.UserName, conv.channel.String())
			return err
		}, true
	case conv.channel != uuid.Nil:
		comment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, id, "")
		if err != nil || comment == nil {
			return nil, false
		}
		return func(emoji string, reactor *dgraphStruct.DgraphUser) error {
			_, err := postBusiness.CreateOrUpdatePostCommentReaction(ctx, &postAdapter.InputUpdateReactionForCommentInPost{EmojiUuid: emoji, Uuid: id}, comment, reactor)
			return err
		}, true
	case !reply:
		chat, err := chatBusiness.GetDgraphChatByUUID(ctx, id)
		if err != nil || chat == nil {
			return nil, false
		}
		return func(emoji string, reactor *dgraphStruct.DgraphUser) error {
			_, err := chatBusiness.CreateOrUpdateChatReaction(ctx, &chatAdapter.InputUpdateReactionForChat{EmojiReactionUuid: emoji, Uuid: id}, chat, reactor, conv.grp)
			return err
		}, true
	}
	comment, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, id, "")
	if err != nil || comment == nil {
		return nil, false
	}
	return func(emoji string, reactor *dgraphStruct.DgraphUser) error {
		_, err := chatBusiness.CreateOrUpdateChatCommentReaction(ctx, &chatAdapter.InputUpdateReactionForCommentInChat{EmojiReactionUuid: emoji, Uuid: id}, comment, reactor)
		return err
	}, true
}

// applyReactions writes m's reactions through put, each emoji's people one
// at a time (business/Reaction's rule is per person and emoji), and says what
// came of them, with the notes for what didn't.
func (s *reactionState) applyReactions(ctx context.Context, m *SlackMessage, put reactionWriter) (reactionTally, []importModels.ImportError) {
	var tally reactionTally
	var notes []importModels.ImportError
	entity := importModels.EntityMessage
	if m.ThreadTs != "" && m.ThreadTs != m.Ts {
		entity = importModels.EntityComment
	}
	for _, r := range m.Reactions {
		emoji, err := s.emoji(ctx, r.Name)
		if err != nil || emoji == "" {
			tally.skippedEmoji += len(r.Users)
			notes = append(notes, importModels.ImportError{
				EntityType: entity, SourceID: m.Ts, Severity: importModels.SeverityWarning, Code: "CUSTOM_EMOJI_UNSUPPORTED",
				Message: "emoji " + r.Name + " not in slack_emoji_map; its reactions were skipped",
			})
			continue
		}
		for _, slackUserId := range r.Users {
			reactorUUID := s.person(ctx, slackUserId)
			if reactorUUID == uuid.Nil {
				tally.skippedUser++
				continue
			}
			reactor := s.lookupFullUser(ctx, reactorUUID.String())
			if reactor == nil || reactor.Uid == "" {
				tally.skippedUser++
				continue
			}
			if err := put(emoji, reactor); err != nil {
				tally.failed++
				notes = append(notes, importModels.ImportError{
					EntityType: entity, SourceID: m.Ts, Severity: importModels.SeverityWarning, Code: "REACTION_IMPORT_FAILED",
					Message: fmt.Sprintf("a :%s: reaction wasn't brought across: %v", r.Name, err),
				})
				continue
			}
			tally.applied++
		}
	}
	return tally, notes
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

	// Read once a pass: emoji names -> emoji ids, Slack users -> OneCamp
	// uuids (uuid.Nil for none).
	emojis map[string]string
	people map[string]uuid.UUID
	// The current batch's messages and replies -> what the import made.
	messages, replies map[string]uuid.UUID
}

// prefetch reads what the reactions in a batch of messages go on, and the
// reactors not yet known, in at most three statements.
func (s *reactionState) prefetch(ctx context.Context, batch []*SlackMessage) {
	var msgs, replies, reactors []string
	seen := map[string]bool{}
	for _, m := range batch {
		if len(m.Reactions) == 0 {
			continue
		}
		if m.ThreadTs != "" && m.ThreadTs != m.Ts {
			replies = append(replies, m.Ts)
		} else {
			msgs = append(msgs, m.Ts)
		}
		for _, r := range m.Reactions {
			for _, u := range r.Users {
				if _, known := s.people[u]; !known && !seen[u] {
					seen[u] = true
					reactors = append(reactors, u)
				}
			}
		}
	}
	var err error
	if s.messages, err = importModels.LookupIdMappingsBatch(ctx, s.importId, importModels.EntityMessage, msgs); err != nil {
		s.messages = nil
	}
	if s.replies, err = importModels.LookupIdMappingsBatch(ctx, s.importId, importModels.EntityComment, replies); err != nil {
		s.replies = nil
	}
	if found, err := importModels.LookupIdMappingsBatch(ctx, s.importId, importModels.EntityUser, reactors); err == nil {
		for _, u := range reactors {
			s.people[u] = found[u]
		}
	}
}

// mapping is what the import made for a message (EntityMessage) or a
// reply (EntityComment), or uuid.Nil.
func (s *reactionState) mapping(ctx context.Context, entityType, ts string) uuid.UUID {
	m := s.messages
	if entityType == importModels.EntityComment {
		m = s.replies
	}
	if m != nil {
		return m[ts]
	}
	id, _ := importModels.LookupIdMapping(ctx, s.importId, entityType, ts)
	return id
}

// person is the OneCamp person a Slack user came across as, or uuid.Nil.
func (s *reactionState) person(ctx context.Context, slackUserId string) uuid.UUID {
	if id, ok := s.people[slackUserId]; ok {
		return id
	}
	id, err := importModels.LookupIdMapping(ctx, s.importId, importModels.EntityUser, slackUserId)
	if err == nil {
		s.people[slackUserId] = id
	}
	return id
}

// emoji is the OneCamp emoji for a Slack emoji name ("" for none), read
// once a pass.
func (s *reactionState) emoji(ctx context.Context, name string) (string, error) {
	if id, ok := s.emojis[name]; ok {
		return id, nil
	}
	id, err := importModels.LookupSlackEmoji(ctx, name)
	if err == nil {
		s.emojis[name] = id
	}
	return id, err
}

func newReactionState(importId uuid.UUID) *reactionState {
	return &reactionState{
		importId:    importId,
		dgUsers:     make(map[string]*dgraphStruct.DgraphUser, 64),
		authors:     make(map[string]string, 64),
		authorUUIDs: make(map[string]string, 64),
		emojis:      make(map[string]string, 64),
		people:      make(map[string]uuid.UUID, 64),
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

	authorUUID := s.person(ctx, slackUserId)
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
