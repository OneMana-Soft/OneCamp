package botpost

// Streamed bot COMMENT replies: the in-thread analog of stream.go. Instead of
// creating a top-level channel post and live-editing it, this creates a Comment
// on an existing post and progressively edits it as the model generates — so an
// @mentioned AI teammate "types" its answer directly in the thread of the
// message that summoned it (Slack-class threaded reply), rather than posting a
// new top-level message.
//
// It reuses the exact same mechanics + guarantees as ChannelStream:
//   - one persisted bot comment is created lazily on the first token;
//   - each subsequent token batch republishes the comment over MQTT as a
//     TYPE_UPDATE edit (throttled) — the same live-edit path the FE already
//     renders for an edited comment (updateChannelCommentByCommentUUID);
//   - Finalize writes the final text to Dgraph (upsert on the same
//     comment_uuid) and OpenSearch so a reload shows the complete answer.
//
// Loop-safe: like every botpost write it emits NO post.created / comment
// workspace event, so a streamed reply can never re-trigger the coworker or an
// agent. If a run is throttled/circuit-broken before any token, no comment is
// ever created (the caller stays silent) — there is no orphan placeholder.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
)

// ReplyStream is the surface-agnostic contract a caller (the AI coworker) uses
// to stream a reply live, without caring whether it lands as a top-level post
// (ChannelStream) or an in-thread comment (PostCommentStream). Both
// implementations are safe for sequential use by one producing goroutine.
type ReplyStream interface {
	// Push updates the live reply with the latest running (plain-text) answer.
	Push(ctx context.Context, runningText string)
	// Finalize commits the complete answer (persisted) and returns identifiers.
	Finalize(ctx context.Context, finalText string) (*Result, error)
	// Abort discards an in-progress stream (e.g. a transient throttle).
	Abort(ctx context.Context)
}

// Compile-time assertions that both stream types satisfy the contract.
var (
	_ ReplyStream = (*ChannelStream)(nil)
	_ ReplyStream = (*PostCommentStream)(nil)
)

// PostCommentStream is a single in-progress streamed bot reply rendered as a
// comment on one post. Create it with BeginPostCommentStream, feed running text
// with Push, and end with Finalize (the answer) or Abort (discard).
type PostCommentStream struct {
	bot           *userBusiness.BotIdentity
	displayName   string
	postUUID      uuid.UUID
	postAuthorUID string // Dgraph uid of the parent post's author (ContentAddedBy)
	channelUUID   string
	channelName   string

	mu          sync.Mutex
	commentUUID uuid.UUID // zero until the first Push creates the comment
	created     bool
	finalized   bool
	lastPublish time.Time
	lastHTML    string
}

// BeginPostCommentStream prepares a streamed in-thread reply — a comment on
// postUUID — authored by bot (its own name/avatar). It does NOT write anything
// yet; the comment is created lazily on the first Push, so a run that produces
// no content (throttled before a token) leaves no placeholder. displayName
// defaults to the bot's own name. bot must be non-nil and resolved; a missing
// parent post is an error so the caller can fall back to a top-level reply.
func BeginPostCommentStream(ctx context.Context, postUUID uuid.UUID, bot *userBusiness.BotIdentity, displayName string) (*PostCommentStream, error) {
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("bot principal is not available")
	}
	postInfo, err := postDomain.GetDgraphPostByUUID(ctx, postUUID.String(), bot.DgraphUID)
	if err != nil || postInfo == nil || postInfo.PostBy == nil || postInfo.Channel == nil {
		if err == nil {
			err = fmt.Errorf("target post not found")
		}
		return nil, err
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = bot.Name
	}
	return &PostCommentStream{
		bot:           bot,
		displayName:   displayName,
		postUUID:      postUUID,
		postAuthorUID: postInfo.PostBy.Uid,
		channelUUID:   postInfo.Channel.Uuid,
		channelName:   postInfo.Channel.Name,
	}, nil
}

// Push updates the live reply with the latest running (plain-text) answer. The
// first non-empty Push creates the persisted comment and broadcasts its
// creation; subsequent calls republish it as a throttled live edit. Empty or
// unchanged text is ignored.
func (s *PostCommentStream) Push(ctx context.Context, runningText string) {
	runningText = strings.TrimSpace(runningText)
	if runningText == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}

	if !s.created {
		if err := s.create(ctx, runningText); err != nil {
			helpers.LogErrorWithContext(ctx, "botpost comment stream: create failed: %+v", err)
		}
		return
	}

	now := time.Now()
	if now.Sub(s.lastPublish) < streamEditThrottle {
		return // throttle intermediate edits
	}
	s.publishEdit(streamHTML(runningText, true), false)
	s.lastPublish = now
}

// Finalize writes the complete answer as the comment's final text (persisted to
// Dgraph + OpenSearch) and publishes the last edit. If no comment was created
// yet (single-shot answer, or Push never called), it creates the comment now
// with the final text. Returns the identifiers. A finalized stream ignores
// further Push/Finalize.
func (s *PostCommentStream) Finalize(ctx context.Context, finalText string) (*Result, error) {
	finalText = strings.TrimSpace(finalText)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return nil, fmt.Errorf("stream already finalized")
	}
	s.finalized = true

	finalHTML := streamHTML(finalText, false)
	if finalHTML == "" {
		finalHTML = "<p></p>"
	}

	if !s.created {
		if err := s.writeComment(ctx, finalHTML, true); err != nil {
			return nil, err
		}
		s.publishCreate(finalHTML)
		s.indexFinal(finalText)
		return s.result(finalHTML), nil
	}

	if err := s.writeComment(ctx, finalHTML, false); err != nil {
		return nil, err
	}
	s.publishEdit(finalHTML, true)
	s.indexFinal(finalText)
	return s.result(finalHTML), nil
}

// Abort discards an in-progress stream. If a comment was already created it is
// hard deleted and a delete is broadcast (so a half-streamed reply does not
// linger when, e.g., a circuit opens mid-stream). A no-op if nothing was
// created.
func (s *PostCommentStream) Abort(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized || !s.created {
		s.finalized = true
		return
	}
	s.finalized = true
	if err := commentDomain.HardDeleteCommentByUUID(ctx, s.commentUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost comment stream: abort delete failed: %+v", err)
	}
	mqttBusiness.PublishPostComment(&mqttStruct.MqttPostComment{
		Type:        mqttStruct.TYPE_DELETE,
		CommentUuid: s.commentUUID.String(),
		PostUuid:    s.postUUID.String(),
		ChannelUuid: s.channelUUID,
		UserUuid:    s.bot.UUID,
	}, s.channelUUID)
}

// create persists the comment (Postgres + Dgraph) with the first streamed text
// and broadcasts its creation. Caller holds s.mu.
func (s *PostCommentStream) create(ctx context.Context, runningText string) error {
	htmlText := streamHTML(runningText, true)
	if err := s.writeComment(ctx, htmlText, true); err != nil {
		return err
	}
	s.publishCreate(htmlText)
	s.lastPublish = time.Now()
	return nil
}

// writeComment creates (createPG=true, first write) or upserts the bot comment
// node with the given HTML. The Dgraph upsert is keyed on comment_uuid, so the
// same node is updated on finalize. Caller holds s.mu.
func (s *PostCommentStream) writeComment(ctx context.Context, htmlText string, createPG bool) error {
	now := helpers.CreateTimeOrNow(ctx)
	zero := time.Time{}
	if createPG {
		s.commentUUID = uuid.New()
		if err := commentDomain.CreateComment(ctx, s.commentUUID, s.bot.UserID, now); err != nil {
			return err
		}
		s.created = true
	}
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:  "uid(po)",
		Uuid: s.postUUID.String(),
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  s.commentUUID.String(),
				Text:  htmlText,
				Post: &dgraphStruct.DgraphPost{
					Uid: "uid(po)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: s.postAuthorUID,
				},
				CreatedAt: &now,
				DeletedAt: &zero,
				CommentBy: &dgraphStruct.DgraphUser{
					Uid: s.bot.DgraphUID,
				},
			},
		},
	}
	if _, err := commentDomain.CreateDgraphCommentInAPost(ctx, &dgraphPost, s.commentUUID.String()); err != nil {
		if createPG {
			_ = commentDomain.HardDeleteCommentByUUID(ctx, s.commentUUID)
			s.created = false
		}
		return err
	}
	return nil
}

func (s *PostCommentStream) profileKey() *string {
	if s.bot.ProfileKey == "" {
		return nil
	}
	pk := s.bot.ProfileKey
	return &pk
}

func (s *PostCommentStream) publishCreate(htmlText string) {
	// lastHTML tracks the last BROADCAST state (not the last persisted text), so
	// publishEdit can de-dupe live edits without ever suppressing the final
	// broadcast.
	s.lastHTML = htmlText
	now := time.Now()
	mqttBusiness.PublishPostComment(&mqttStruct.MqttPostComment{
		Type:           mqttStruct.TYPE_CREATE,
		CommentUuid:    s.commentUUID.String(),
		PostUuid:       s.postUUID.String(),
		HTMLText:       htmlText,
		ChannelUuid:    s.channelUUID,
		CreatedAt:      &now,
		UserUuid:       s.bot.UUID,
		UserName:       s.displayName,
		UserProfileKey: s.profileKey(),
		IsBot:          true,
	}, s.channelUUID)
}

// publishEdit broadcasts a live TYPE_UPDATE edit of the streaming comment (no DB
// write — Finalize persists). force=true always sends (used for the final
// answer so it can never be suppressed); otherwise an unchanged edit is
// de-duped. Caller holds s.mu.
func (s *PostCommentStream) publishEdit(htmlText string, force bool) {
	if !force && htmlText == s.lastHTML {
		return
	}
	s.lastHTML = htmlText
	now := time.Now()
	mqttBusiness.PublishPostComment(&mqttStruct.MqttPostComment{
		Type:        mqttStruct.TYPE_UPDATE,
		CommentUuid: s.commentUUID.String(),
		PostUuid:    s.postUUID.String(),
		HTMLText:    htmlText,
		ChannelUuid: s.channelUUID,
		UpdatedAt:   &now,
		UserUuid:    s.bot.UUID,
	}, s.channelUUID)
}

func (s *PostCommentStream) indexFinal(finalText string) {
	now := time.Now()
	osComment := &openSearchStruct.OpenSearchComment{
		Uuid:                  s.commentUUID.String(),
		CommentBody:           finalText,
		CommentByUserUuid:     s.bot.UUID,
		CommentByUserFullName: s.displayName,
		CommentByProfile:      s.profileKey(),
		CommentChannelUuid:    s.channelUUID,
		CommentChannelName:    s.channelName,
		CommentPostUuid:       s.postUUID.String(),
		CommentCreatedAt:      now.Unix(),
	}
	go commentDomain.CreatePostCommentWithAttachmentsInOpenSearch(osComment, nil)
}

func (s *PostCommentStream) result(htmlText string) *Result {
	return &Result{
		PostUUID:    s.postUUID.String(),
		CommentUUID: s.commentUUID.String(),
		BotUserUUID: s.bot.UUID,
		BotDgraphID: s.bot.DgraphUID,
		ChannelName: s.channelName,
		HTMLText:    htmlText,
	}
}
