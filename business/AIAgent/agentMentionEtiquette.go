package business

// Mention etiquette: lightweight, deterministic handling of common social
// @mentions so an agent (or the AI coworker) does not waste tokens or annoy
// people when the message is just a greeting or a clear request to stop.
//
// This is intentionally rule-based, not LLM-based: it must be fast, cheap,
// never hallucinate, and never itself become a source of spam. A greeting is
// answered once and then the thread goes quiet until the person asks for
// something real. A dismissal is honoured immediately — including cancelling
// any open durable run on the same surface.

import (
	"context"
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// MentionIntent is the coarse class of a message that @mentions an agent.
type MentionIntent int

const (
	// IntentTask means the message asks the agent to do something.
	IntentTask MentionIntent = iota
	// IntentGreeting means the message is just a social greeting.
	IntentGreeting
	// IntentDismiss means the message tells the agent to stop/ignore/close.
	IntentDismiss
)

// greetingPatterns matches standalone greetings and short greeting + name
// combinations ("hi", "hi bot", "hello there", "good morning @bot").
// Deliberately conservative: "hi, can you summarize?" is a task, not a greeting.
var greetingPatterns = []string{
	`^(hi|hello|hey|hiya|howdy|yo|greetings|good morning|good afternoon|good evening|what's up|whats up|sup)(\s+[^!?.,]*?)?[!?.,]*$`,
}

// dismissalPattern matches explicit requests to stop, cancel, or dismiss the
// agent in the current thread. These are honoured silently; no reply is posted.
// It allows polite trailers ("please", "thanks", "it", "that") but rejects a
// dismissal word followed by a new request ("stop and tell me ...").
const dismissalPattern = `^(please\s+)?(stop|close|dismiss|cancel|abort|quit|end|done|ignore|shut up|go away|never mind|nevermind|nvm|that's all|that is all|all good|no thanks?|no thank you|not now|leave it|forget it)([\s,]+(please|thanks?|thank you|now|it|that|this|doing that|the current run))*$`

var (
	greetingRE  = regexp.MustCompile(strings.Join(greetingPatterns, "|"))
	dismissalRE = regexp.MustCompile(dismissalPattern)
)

// genericAgentLabels are words left behind after stripping a bare @mention of
// an AI agent ("@Standup Bot" -> "Bot"). They carry no intent and should be
// treated the same as an empty message.
var genericAgentLabels = map[string]bool{
	"bot": true, "agent": true, "ai": true, "assistant": true, "onecamp": true,
}

// classifyMentionIntent categorises a mention message. It strips the matched
// agent's name/handle, other @handles, and leading/trailing punctuation so
// formatting does not change the outcome.
func ClassifyMentionIntent(text string, agent *model.AiAgent) MentionIntent {
	clean := stripMentions(text)
	if agent != nil {
		clean = stripAgentReferences(clean, agent)
	}
	clean = strings.ToLower(strings.TrimSpace(clean))
	clean = strings.Trim(clean, "!?.,;:")
	if clean == "" || genericAgentLabels[clean] {
		return IntentGreeting // a bare @mention is treated like a wave
	}
	if dismissalRE.MatchString(clean) {
		return IntentDismiss
	}
	if greetingRE.MatchString(clean) {
		return IntentGreeting
	}
	return IntentTask
}

// stripMentions removes @handle chips and plain @handles from text so the
// intent classifier sees only what the person wrote.
func stripMentions(text string) string {
	// Remove HTML mention spans if present: <span data-mention-id="...">@Name</span>
	s := regexp.MustCompile(`<span[^>]*data-mention[^>]*>.*?</span>`).ReplaceAllString(text, "")
	// Remove plain @handles (word chars, spaces inside names, dots, dashes).
	s = regexp.MustCompile(`@\S+`).ReplaceAllString(s, "")
	return helpers.HTMLToPlainText(s)
}

// stripAgentReferences removes an agent's display name and configured handles
// from text after mention parsing has already identified which agent matched.
func stripAgentReferences(text string, agent *model.AiAgent) string {
	if agent == nil {
		return text
	}
	s := text
	for _, h := range mentionHandles(agent) {
		if h == "" {
			continue
		}
		s = regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(h)+`\b`).ReplaceAllString(s, "")
	}
	if agent.Name != "" {
		s = regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(agent.Name)+`\b`).ReplaceAllString(s, "")
	}
	return s
}

// agentGreetingReply returns a friendly, low-pressure greeting for an agent.
func agentGreetingReply(agentName string) string {
	return "Hi there! I'm " + agentName + ". What would you like me to help you with?"
}

// cancelOpenAgentRunsForSource requests cancellation of every open durable task
// for the given source. Best-effort: failures are logged but do not block the
// dismissal from being honoured.
func cancelOpenAgentRunsForSource(ctx context.Context, sourceType, sourceID string, by uuid.UUID) {
	if strings.TrimSpace(sourceID) == "" {
		return
	}
	tasks, err := model.ListAgentTasksBySource(ctx, sourceType, sourceID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: list tasks for cancel: %v", err)
		return
	}
	for _, t := range tasks {
		open := false
		for _, s := range model.OpenTaskStates {
			if t.State == s {
				open = true
				break
			}
		}
		if !open {
			continue
		}
		status, cerr := model.RequestAgentTaskCancel(ctx, t.Id, by)
		if cerr != nil {
			helpers.LogErrorWithContext(ctx, "agentTriggers: request cancel for task %s: %v", t.Id, cerr)
			continue
		}
		helpers.LogInfoWithContext(ctx, "agentTriggers: dismissed task %s (status=%s)", t.Id, status)
	}
}
