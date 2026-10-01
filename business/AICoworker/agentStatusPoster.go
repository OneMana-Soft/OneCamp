package aicoworker

// Chat status poster for durable agent runs (async-mentions spec Task 3).
//
// business/AIAgent defines the surface-agnostic StatusPoster and drives the
// evolving in-thread "On it → working… → result" comment, but it deliberately
// does NOT import business/Chat. So the coworker package (which owns chat
// transport) supplies the group/DM implementation and registers it as a factory
// at startup — the same dependency-injection pattern used for event listeners.

import (
	"context"
	"strings"
	"sync"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	agentbusiness "github.com/akashc777/OneCamp/business/AIAgent"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// chatStatusProgressThrottle bounds how often the live "working…" line is edited
// so a fast multi-step run doesn't churn the chat thread / MQTT.
const chatStatusProgressThrottle = 3 * time.Second

// newChatStatusPoster builds a StatusPoster for a group/DM chat message,
// authored as the agent's own badged principal. Returns nil when the principal
// can't be resolved, so the durable worker falls back to best-effort one-shot
// posts. Registered with business/AIAgent at startup.
func newChatStatusPoster(ctx context.Context, agentBotUUID, messageID string) agentbusiness.StatusPoster {
	agentBotUUID = strings.TrimSpace(agentBotUUID)
	messageID = strings.TrimSpace(messageID)
	if agentBotUUID == "" || messageID == "" {
		return nil
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, agentBotUUID)
	if err != nil || botInfo == nil {
		return nil
	}
	return &chatCommentStatusPoster{botInfo: botInfo, messageID: messageID}
}

// chatCommentStatusPoster shows the evolving status as one in-thread comment on
// a group/DM chat message. Loop-safe (the create + edit paths emit no workspace
// event). Best-effort: a create/edit failure is logged, never fatal.
type chatCommentStatusPoster struct {
	botInfo   *userModels.UserInfo
	messageID string

	mu           sync.Mutex
	commentUUID  uuid.UUID // zero until the comment is created
	lastProgress time.Time
}

// Set creates the status comment (first call) or edits it to text (the "On it"
// ack and the final result).
func (p *chatCommentStatusPoster) Set(ctx context.Context, text string) {
	if p == nil || strings.TrimSpace(text) == "" {
		return
	}
	html := toChatHTML(text)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil {
		res, err := chatBusiness.PostChatCommentAsBot(ctx, p.botInfo, p.messageID, html)
		if err != nil || res == nil {
			helpers.LogErrorWithContext(ctx, "agent chat status: create comment failed (msg=%s): %v", p.messageID, err)
			return
		}
		if id, perr := uuid.Parse(res.Uuid); perr == nil {
			p.commentUUID = id
		}
		return
	}
	if err := chatBusiness.EditChatCommentAsBot(ctx, p.botInfo, p.messageID, p.commentUUID, html); err != nil {
		helpers.LogErrorWithContext(ctx, "agent chat status: edit comment failed (msg=%s): %v", p.messageID, err)
	}
}

// Progress edits the status comment to a throttled "working…" line. No-op until
// the comment exists (a run that never posted an ack stays quiet).
func (p *chatCommentStatusPoster) Progress(ctx context.Context, tools []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil || time.Since(p.lastProgress) < chatStatusProgressThrottle {
		return
	}
	line := "Working on it…"
	if used := progressToolsLine(tools); used != "" {
		line += " " + used
	}
	p.lastProgress = time.Now()
	if err := chatBusiness.EditChatCommentAsBot(ctx, p.botInfo, p.messageID, p.commentUUID, toChatHTML(line)); err != nil {
		helpers.LogErrorWithContext(ctx, "agent chat status: edit progress failed (msg=%s): %v", p.messageID, err)
	}
}

// progressToolsLine renders a compact "(using: a, b)" hint from the distinct
// tool names used so far, or "" when none. Pure.
func progressToolsLine(tools []string) string {
	seen := make(map[string]bool, len(tools))
	var names []string
	for _, t := range tools {
		t = strings.ReplaceAll(strings.TrimSpace(t), "_", " ")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		names = append(names, t)
		if len(names) >= 4 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "(using: " + strings.Join(names, ", ") + ")"
}
