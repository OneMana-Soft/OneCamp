package business

// Generic status poster for durable agent runs (async-mentions spec Task 3).
//
// A durable run shows ONE evolving in-thread comment as the agent's badged
// principal — "On it… → working (used: …) → result" — instead of a trail of
// placeholders. The worker drives this through the surface-agnostic statusPoster
// interface; a concrete poster per reply surface (task comment, channel-post
// comment, chat comment) knows how to create + edit that one comment.
//
// Layering: the channel-post poster lives here (business/AIAgent already depends
// on botpost). The chat poster needs business/Chat, which AIAgent deliberately
// does NOT import (agentDM/agentGroupChat keep this package chat-free), so the
// coworker package injects a chat poster factory at startup via
// RegisterChatStatusPosterFactory — the same dependency-injection pattern used
// for event listeners. newStatusPoster selects the right poster from a Surface.

import (
	"context"
	"strings"
	"sync"
	"time"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// StatusPoster is the surface-agnostic contract the durable worker uses to show
// live progress: Set() creates-or-edits the single status comment (the "On it"
// ack and the final result), Progress() edits it with a throttled working line.
// taskStatusPoster, postCommentStatusPoster, and the coworker's chat poster all
// satisfy it, so the worker never special-cases a surface. Exported (with
// exported methods) so the coworker package can supply the chat implementation.
type StatusPoster interface {
	Set(ctx context.Context, text string)
	Progress(ctx context.Context, tools []string)
}

// chatStatusPosterFactory builds a StatusPoster for a group/DM chat message. It
// is injected by the coworker package (which owns chat transport) so AIAgent
// stays chat-dependency-free. nil until registered → chat surfaces fall back to
// best-effort one-shot posts.
type chatStatusPosterFactory func(ctx context.Context, agentBotUUID, messageID string) StatusPoster

var chatPosterFactory chatStatusPosterFactory

// RegisterChatStatusPosterFactory wires the coworker's chat status poster.
// Called once at startup; safe before traffic (no locking needed on the hot
// path beyond this single assignment at init).
func RegisterChatStatusPosterFactory(fn chatStatusPosterFactory) { chatPosterFactory = fn }

// newStatusPoster returns the poster for a reply surface, or nil when it can't
// be resolved (the worker then falls back to best-effort one-shot posts, so a
// resolution miss never blocks a run). SurfaceTask is handled by the existing
// newTaskStatusPoster (the task worker constructs it directly), so it is not
// built here.
func newStatusPoster(ctx context.Context, agent *model.AiAgent, surface Surface) StatusPoster {
	switch surface.Kind {
	case SurfaceChannelPost:
		bot := resolveAgentBot(ctx, agent)
		if bot == nil {
			return nil
		}
		postUUID, err := uuid.Parse(surface.PostID)
		if err != nil {
			return nil
		}
		return &postCommentStatusPoster{bot: bot, postUUID: postUUID}
	case SurfaceGroupChat, SurfaceDM:
		if chatPosterFactory == nil {
			return nil
		}
		bot := resolveAgentBot(ctx, agent)
		if bot == nil || surface.MessageID == "" {
			return nil
		}
		return chatPosterFactory(ctx, bot.UUID, surface.MessageID)
	default:
		return nil
	}
}

// resolveAgentBot resolves the agent's own badged principal (provisioning on
// first use), or nil on failure.
func resolveAgentBot(ctx context.Context, agent *model.AiAgent) *userBusiness.BotIdentity {
	bot, err := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey))
	if err != nil || bot == nil || bot.DgraphUID == "" || bot.UUID == "" {
		return nil
	}
	return bot
}

// postCommentStatusPoster shows the evolving status as one in-thread comment on
// a channel post, authored as the agent's badged principal. Loop-safe (botpost
// emits no workspace event). Best-effort: a create/edit failure is logged, never
// fatal.
type postCommentStatusPoster struct {
	bot      *userBusiness.BotIdentity
	postUUID uuid.UUID

	mu           sync.Mutex
	commentUUID  uuid.UUID // zero until the comment is created
	lastProgress time.Time
}

// Set creates the status comment (first call) or edits it to text (the "On it"
// ack and the final result). Plain text — botpost badges/sanitizes it.
func (p *postCommentStatusPoster) Set(ctx context.Context, text string) {
	if p == nil || strings.TrimSpace(text) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil {
		res, err := botpost.PostCommentToPostAsBot(ctx, p.postUUID, text, p.bot)
		if err != nil || res == nil {
			helpers.LogErrorWithContext(ctx, "agent status: create post comment failed (post=%s): %v", p.postUUID, err)
			return
		}
		if id, perr := uuid.Parse(res.CommentUUID); perr == nil {
			p.commentUUID = id
		}
		return
	}
	if _, err := botpost.EditCommentToPostAsBot(ctx, p.postUUID, p.commentUUID, text, p.bot); err != nil {
		helpers.LogErrorWithContext(ctx, "agent status: edit post comment failed (post=%s): %v", p.postUUID, err)
	}
}

// Progress edits the status comment to a throttled "working…" line. No-op until
// the comment exists (a run that never posted an ack stays quiet).
func (p *postCommentStatusPoster) Progress(ctx context.Context, tools []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil || time.Since(p.lastProgress) < taskProgressThrottle {
		return
	}
	line := "Working on it…"
	if tf := toolsUsedFooter(tools); tf != "" {
		line += "\n\n" + tf
	}
	p.lastProgress = time.Now()
	if _, err := botpost.EditCommentToPostAsBot(ctx, p.postUUID, p.commentUUID, line, p.bot); err != nil {
		helpers.LogErrorWithContext(ctx, "agent status: edit progress comment failed (post=%s): %v", p.postUUID, err)
	}
}
