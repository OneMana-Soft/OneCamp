package botpost

// Streamed bot replies: post a message and progressively EDIT it as the model
// generates, so a channel watches the AI "type" (Notion/Slack-class live
// output) instead of waiting for the whole answer and seeing it appear at once.
//
// Mechanics (reuses existing primitives, no parallel machinery):
//   - one persisted bot post is created lazily on the first token;
//   - each subsequent token batch republishes the post over MQTT as a
//     TYPE_UPDATE edit (throttled), exactly the live-edit path the FE already
//     renders for an edited message;
//   - Finalize writes the final text to Dgraph (upsert on the same post_uuid)
//     and OpenSearch so a reload shows the complete answer.
//
// Loop-safe: like every botpost write it emits NO post.created event, so a
// streamed reply can never re-trigger the coworker or an agent. If a run is
// throttled/circuit-broken before any token, no post is ever created (the
// caller simply stays silent) — there is no orphan placeholder.

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
)

// streamEditThrottle bounds how often a streaming reply republishes its edit,
// so a fast token stream cannot flood MQTT / the clients. The final text is
// always published on Finalize regardless of throttle, so the end state is
// exact.
const streamEditThrottle = 200 * time.Millisecond

// streamCursor is appended to in-progress text so the live message reads as
// actively typing.
const streamCursor = "▍"

// ChannelStream is a single in-progress streamed bot reply in one channel.
// Create it with BeginChannelStream, feed running text with Push, and end with
// Finalize (the answer) or Abort (discard, e.g. a transient throttle with no
// content). All methods are safe for sequential use by one producing goroutine;
// an internal mutex guards the throttle/finalize state.
type ChannelStream struct {
	bot         *userBusiness.BotIdentity
	displayName string
	channelUUID uuid.UUID
	channelUID  string
	channelName string

	mu          sync.Mutex
	postUUID    uuid.UUID // zero until the first Push creates the post
	created     bool
	finalized   bool
	lastPublish time.Time
	lastHTML    string
}

// BeginChannelStream prepares a streamed reply in channelUUID authored by bot
// (its own name/avatar + the "AI" badge). It does NOT post anything yet — the
// post is created lazily on the first Push — so a run that produces no content
// (throttled before a token) leaves no placeholder. displayName defaults to the
// bot's own name. bot must be non-nil and resolved.
func BeginChannelStream(ctx context.Context, channelUUID uuid.UUID, bot *userBusiness.BotIdentity, displayName string) (*ChannelStream, error) {
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("bot principal is not available")
	}
	channelDgraph, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), bot.DgraphUID)
	if err != nil || channelDgraph == nil {
		return nil, fmt.Errorf("channel not found")
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = bot.Name
	}
	return &ChannelStream{
		bot:         bot,
		displayName: displayName,
		channelUUID: channelUUID,
		channelUID:  channelDgraph.Uid,
		channelName: channelDgraph.Name,
	}, nil
}

// Push updates the live reply with the latest running (plain-text) answer. The
// first non-empty Push creates the persisted post and broadcasts its creation;
// subsequent calls republish it as a throttled live edit. Empty or unchanged
// text is ignored.
func (s *ChannelStream) Push(ctx context.Context, runningText string) {
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
			helpers.LogErrorWithContext(ctx, "botpost stream: create failed: %+v", err)
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

// Finalize writes the complete answer as the post's final text (persisted to
// Dgraph + OpenSearch) and publishes the last edit. If no post was created yet
// (the whole answer arrived in one shot, or Push was never called), it creates
// the post now with the final text. Returns the post's identifiers. A finalized
// stream ignores further Push/Finalize.
func (s *ChannelStream) Finalize(ctx context.Context, finalText string) (*Result, error) {
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
		// Nothing streamed (single-shot answer): create the post directly with
		// the final text and index it.
		if err := s.writePost(ctx, finalHTML, true); err != nil {
			return nil, err
		}
		s.publishCreate(finalHTML)
		s.indexFinal(finalText)
		return s.result(finalHTML), nil
	}

	// Update the existing streamed post to its final text + index it. The final
	// edit is broadcast unconditionally (force) so the answer can never be
	// suppressed by the live-edit de-dupe.
	if err := s.writePost(ctx, finalHTML, false); err != nil {
		return nil, err
	}
	s.publishEdit(finalHTML, true)
	s.indexFinal(finalText)
	return s.result(finalHTML), nil
}

// Abort discards an in-progress stream. If a post was already created it is hard
// deleted and a delete is broadcast (so a half-streamed reply does not linger
// when, e.g., a circuit opens mid-stream). A no-op if nothing was created.
func (s *ChannelStream) Abort(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized || !s.created {
		s.finalized = true
		return
	}
	s.finalized = true
	if err := postDomain.HardDeletePostByUUIUD(ctx, s.postUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost stream: abort delete failed: %+v", err)
	}
	mqttBusiness.PublishPost(&mqttStruct.MqttPost{
		Type:            mqttStruct.TYPE_DELETE,
		PostUuid:        s.postUUID.String(),
		PostChannelUuid: s.channelUUID.String(),
		PostByUserUuid:  s.bot.UUID,
	}, s.channelUUID.String())
}

// create persists the post (Postgres + Dgraph) with the first streamed text and
// broadcasts its creation. Caller holds s.mu.
func (s *ChannelStream) create(ctx context.Context, runningText string) error {
	htmlText := streamHTML(runningText, true)
	if err := s.writePost(ctx, htmlText, true); err != nil {
		return err
	}
	s.publishCreate(htmlText)
	s.lastPublish = time.Now()
	return nil
}

// writePost creates (createPG=true, first write) or upserts the bot post node
// with the given HTML. The Dgraph upsert is keyed on post_uuid, so the same
// node is updated on finalize. Caller holds s.mu.
func (s *ChannelStream) writePost(ctx context.Context, htmlText string, createPG bool) error {
	now := helpers.CreateTimeOrNow(ctx)
	zero := time.Time{}
	if createPG {
		s.postUUID = uuid.New()
		if err := postDomain.CreatePost(ctx, s.postUUID, s.bot.UserID, s.channelUUID); err != nil {
			return err
		}
		s.created = true
	}
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:   "uid(po)",
		Uuid:  s.postUUID.String(),
		DType: []string{"Post"},
		Text:  htmlText,
		PostBy: &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   s.bot.DgraphUID,
			Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)", DType: []string{"Post"}}},
		},
		Channel: &dgraphStruct.DgraphChannel{
			DType: []string{"Channel"},
			Uid:   s.channelUID,
			Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)", DType: []string{"Post"}}},
		},
		CreatedAt: &now,
		DeletedAt: &zero,
	}
	if _, err := postDomain.CreateOrUpdateDgraphPost(ctx, &dgraphPost); err != nil {
		if createPG {
			_ = postDomain.HardDeletePostByUUIUD(ctx, s.postUUID)
			s.created = false
		}
		return err
	}
	return nil
}

func (s *ChannelStream) profileKey() *string {
	if s.bot.ProfileKey == "" {
		return nil
	}
	pk := s.bot.ProfileKey
	return &pk
}

func (s *ChannelStream) publishCreate(htmlText string) {
	// lastHTML tracks the last BROADCAST state (not the last persisted text), so
	// publishEdit can de-dupe live edits without ever suppressing the final
	// broadcast.
	s.lastHTML = htmlText
	now := time.Now()
	mqttBusiness.PublishPost(&mqttStruct.MqttPost{
		Type:             mqttStruct.TYPE_CREATE,
		PostHtmlText:     htmlText,
		PostCreatedAt:    &now,
		PostByUserUuid:   s.bot.UUID,
		PostByProfileKey: s.profileKey(),
		PostByUserName:   s.displayName,
		PostByIsBot:      true,
		PostChannelUuid:  s.channelUUID.String(),
		PostUuid:         s.postUUID.String(),
	}, s.channelUUID.String())
}

// publishEdit broadcasts a live TYPE_UPDATE edit of the streaming message
// (no DB write — Finalize persists). force=true always sends (used for the
// final answer so it can never be suppressed); otherwise an unchanged edit is
// de-duped. lastHTML tracks the last broadcast state. Caller holds s.mu.
func (s *ChannelStream) publishEdit(htmlText string, force bool) {
	if !force && htmlText == s.lastHTML {
		return
	}
	s.lastHTML = htmlText
	now := time.Now()
	mqttBusiness.PublishPost(&mqttStruct.MqttPost{
		Type:            mqttStruct.TYPE_UPDATE,
		PostUuid:        s.postUUID.String(),
		PostHtmlText:    htmlText,
		PostUpdatedAt:   &now,
		PostChannelUuid: s.channelUUID.String(),
		PostByUserUuid:  s.bot.UUID,
	}, s.channelUUID.String())
}

func (s *ChannelStream) indexFinal(finalText string) {
	now := time.Now()
	osPost := &openSearchStruct.OpenSearchPost{
		Uuid:               s.postUUID.String(),
		PostBody:           finalText,
		PostChannelUuid:    s.channelUUID.String(),
		PostCreatedAt:      now.Unix(),
		PostByUserUuid:     s.bot.UUID,
		PostByProfile:      s.profileKey(),
		PostByUserFullName: s.displayName,
		PostChannelName:    s.channelName,
	}
	go postDomain.CreatePostWithAttachmentsInOpenSearch(osPost, nil)
}

func (s *ChannelStream) result(htmlText string) *Result {
	return &Result{
		PostUUID:    s.postUUID.String(),
		BotUserUUID: s.bot.UUID,
		BotDgraphID: s.bot.DgraphUID,
		ChannelName: s.channelName,
		HTMLText:    htmlText,
	}
}

// streamHTML renders plain-text (possibly multi-paragraph) model output as safe
// chat HTML: each paragraph escaped and wrapped in <p>, single newlines as
// <br>, a closed ```chart block as an inline chart embed node, and a ```diff
// block as a diff embed. When typing is true a blinking cursor glyph is
// appended to the last run of text so the live message reads as actively
// generating. Mirrors the coworker's plain->HTML renderer so the streamed and
// final shapes match.
func streamHTML(text string, typing bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// A closed ```chart block becomes a chart embed node; the runs of text
	// around it render as paragraphs, with the typing cursor on the last one.
	return RenderWithCharts(text, func(run string, last bool) string {
		return renderParagraphs(run, typing && last)
	})
}

// renderParagraphs is the plain-text → chat HTML renderer: each blank-line
// separated paragraph escaped and wrapped in <p>, single newlines as <br/>,
// with an optional trailing typing cursor on the last paragraph.
func renderParagraphs(text string, typing bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	paras := strings.Split(text, "\n\n")
	// Index of the last non-empty paragraph (where the cursor should land).
	lastNonEmpty := -1
	for i, para := range paras {
		if strings.TrimSpace(para) != "" {
			lastNonEmpty = i
		}
	}
	for i, para := range paras {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		escaped := strings.ReplaceAll(html.EscapeString(para), "\n", "<br/>")
		b.WriteString("<p>")
		b.WriteString(escaped)
		if typing && i == lastNonEmpty {
			b.WriteString(streamCursor)
		}
		b.WriteString("</p>")
	}
	return b.String()
}
