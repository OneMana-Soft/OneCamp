package ai

// Telling a person when their answer was built from less than they asked about.
//
// Two things now shorten a prompt behind the scenes. The budget trims context and
// history pre-emptively so the request fits the model's window, and the overflow rescue
// shrinks it again if the provider refuses it anyway. Both make the difference between
// a working answer and no answer, and both are silent — the model is told its context
// was cut, and the person reading the reply is not.
//
// Silent degradation is worse than a visible limit. Someone who can see that half the
// thread was dropped knows to narrow the question or pick a bigger model; someone who
// cannot see it just concludes the AI is unreliable, which is the same outcome as it
// being broken and harder to diagnose.
//
// Follows the WithUsageSink convention in usage.go for the same reason it exists: the
// LLMProvider signature returns text, so a caller attaches a sink once and reads what
// happened afterwards, and every path without a sink is unaffected.

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ContextNotice is what happened to a prompt on its way to the model.
type ContextNotice struct {
	// Trimmed is true when the assembled context or history was cut to fit the
	// model's window before the request was made.
	Trimmed bool `json:"trimmed,omitempty"`
	// Rescued is true when the provider REFUSED the prompt and it was shrunk and
	// retried. A stronger signal than Trimmed: it means our budget was wrong, not
	// merely tight, so more was dropped than planned.
	Rescued bool `json:"rescued,omitempty"`
	// DroppedMessages counts whole conversation turns removed by a rescue. 0 when the
	// shrink only clipped text rather than dropping turns.
	DroppedMessages int `json:"dropped_messages,omitempty"`
	// Degraded names capabilities that were configured but unreachable while this
	// answer was produced, so the answer was built from less than the workspace
	// actually has.
	//
	// This existed only in the server log. A connector whose credential could no
	// longer be decrypted contributed no tools, the assistant answered without it,
	// and the person who got the thinner answer was never told there was a thicker
	// one available. A silently degraded answer is worse than a refused one:
	// it spends the credibility of every answer that did work.
	Degraded []string `json:"degraded,omitempty"`
}

// Any reports whether anything worth telling a person about happened.
func (n ContextNotice) Any() bool { return n.Trimmed || n.Rescued || len(n.Degraded) > 0 }

// Message is the user-facing sentence, or "" when there is nothing to say.
//
// Deliberately short and non-alarming: this is a normal, successful answer that used
// less input than it could have. It names the cause and the lever, and nothing else —
// a warning that reads like an error trains people to ignore warnings.
func (n ContextNotice) Message() string {
	switch {
	// Named first: something the workspace can fix outranks something it merely
	// had to work around.
	case len(n.Degraded) > 0:
		return "This answer was produced without " + joinHumanly(n.Degraded) + ", which " + verbFor(len(n.Degraded)) + " configured but unreachable. An admin can reconnect it in Admin settings."
	case n.Rescued:
		return "This answer used a shortened view of the conversation — it was longer than the model could accept. Ask about a smaller part, or pick a model with a larger context window."
	case n.Trimmed:
		return "This answer used a shortened view of the conversation to fit the model's context window."
	default:
		return ""
	}
}

type noticeSink struct {
	mu sync.Mutex
	n  ContextNotice
	// pending is what was unreachable when this request began, and asked is
	// what the person wanted, once some path knows. Held rather than folded
	// in immediately because the two facts arrive in that order: the sink is
	// attached before the question has been read, and whether a broken
	// connector mattered is a question about the question.
	pending []DegradedCapability
	asked   string
}

type noticeCtxKeyT struct{}

var noticeCtxKey noticeCtxKeyT

// WithContextNoticeSink returns a context that records prompt shortening. Idempotent,
// so nested calls share one record rather than each starting a fresh one.
//
// Attaching a sink is the declaration that this path will tell a person what
// happened, which makes it the one place worth asking what is currently broken.
// Nine surfaces attach one; asking here rather than at each of them is the
// difference between one answer and nine that drift apart.
func WithContextNoticeSink(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(noticeCtxKey) != nil {
		return ctx
	}
	return context.WithValue(ctx, noticeCtxKey, &noticeSink{pending: degradations()})
}

// NoteQuestion records what this answer was asked, so a capability that was
// unreachable can be mentioned when it would have mattered and left out when
// it would not.
//
// Optional by design: a path that never calls this has every unreachable
// capability named, which is what every path did before and is the direction
// that cannot hide a thinner answer. Calling it can only narrow the list.
func NoteQuestion(ctx context.Context, question string) {
	if ctx == nil || strings.TrimSpace(question) == "" {
		return
	}
	s, _ := ctx.Value(noticeCtxKey).(*noticeSink)
	if s == nil {
		return
	}
	s.mu.Lock()
	// The first question wins. A path that assembles a prompt out of several
	// parts would otherwise end up matching against the last fragment.
	if s.asked == "" {
		s.asked = question
	}
	s.mu.Unlock()
}

// reportContextNotice folds one observation into the sink, if a caller attached one.
// No-op otherwise, so paths that do not report to a user pay nothing.
//
// Merges rather than overwrites: one request can be trimmed pre-emptively AND rescued
// afterwards, and the stronger fact must survive the weaker one.
func reportContextNotice(ctx context.Context, n ContextNotice) {
	if ctx == nil || !n.Any() {
		return
	}
	s, _ := ctx.Value(noticeCtxKey).(*noticeSink)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.n.Trimmed = s.n.Trimmed || n.Trimmed
	s.n.Rescued = s.n.Rescued || n.Rescued
	s.n.DroppedMessages += n.DroppedMessages
	for _, d := range n.Degraded {
		if !slices.Contains(s.n.Degraded, d) {
			s.n.Degraded = append(s.n.Degraded, d)
		}
	}
	s.mu.Unlock()
}

// maxNamedCapabilities is how many unreachable capabilities are named before the
// sentence stops listing and starts counting. A workspace with a dozen broken
// connectors has one problem, not a dozen, and a paragraph of names is a notice
// nobody finishes reading.
const maxNamedCapabilities = 3

// joinHumanly renders a list the way a sentence needs it rather than the way a
// slice prints: "A", "A and B", "A, B and C", then "A, B, C and 4 others".
func joinHumanly(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	if len(items) > maxNamedCapabilities {
		rest := len(items) - maxNamedCapabilities
		noun := "others"
		if rest == 1 {
			noun = "other"
		}
		return strings.Join(items[:maxNamedCapabilities], ", ") + " and " + strconv.Itoa(rest) + " " + noun
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// verbFor agrees the verb with the count, so the sentence reads correctly for
// one unreachable connector and for three.
func verbFor(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// TakeContextNotice returns what was recorded and resets the sink, so a caller can
// attribute one notice per answer. Zero value when no sink is attached.
func TakeContextNotice(ctx context.Context) ContextNotice {
	if ctx == nil {
		return ContextNotice{}
	}
	s, _ := ctx.Value(noticeCtxKey).(*noticeSink)
	if s == nil {
		return ContextNotice{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.n
	// Resolved here rather than at attach time: this is the first moment both
	// halves are known, what was broken and what was asked.
	for _, name := range relevantDegradations(s.pending, s.asked) {
		if !slices.Contains(n.Degraded, name) {
			n.Degraded = append(n.Degraded, name)
		}
	}
	s.n = ContextNotice{}
	s.pending, s.asked = nil, ""
	return n
}

// AppendNoticeFootnote returns text with the notice added as a quiet italic line, or
// text unchanged when there is nothing to report.
//
// For surfaces whose "response" is a posted message rather than an HTTP payload — the
// in-channel coworker, DM and group replies. Those have no envelope to carry a field, so
// the footnote has to live in the message, and a separated italic line is how a chat
// message says something parenthetical. Markdown because the message renderer already
// handles it.
//
// One function rather than a string built at each reply site: three surfaces posting
// three slightly different sentences about the same condition is how wording drifts, and
// the wording is the whole point of telling someone.
func AppendNoticeFootnote(text string, n ContextNotice) string {
	msg := n.Message()
	if msg == "" {
		return text
	}
	trimmed := strings.TrimRight(text, " \n")
	if trimmed == "" {
		// Nothing was answered; a footnote about how it was answered would be absurd.
		return text
	}
	return trimmed + "\n\n_" + msg + "_"
}

// TruncateForPrompt is TruncateToTokenBudget that also records the cut, so a caller can
// tell the person their answer was built from less than the whole thread.
//
// A separate method rather than reporting from TruncateToTokenBudget itself because that
// one takes no context and is used for things that are not prompts. Callers assembling a
// prompt for a human use this; everything else is unchanged.
func (l ModelLimits) TruncateForPrompt(ctx context.Context, s string, budget int) string {
	out := l.TruncateToTokenBudget(s, budget)
	if len(out) != len(s) {
		reportContextNotice(ctx, ContextNotice{Trimmed: true})
	}
	return out
}

// TrimHistoryForPrompt is TrimHistoryToBudget that also records dropped turns.
func (l ModelLimits) TrimHistoryForPrompt(ctx context.Context, history []ChatMessage, budget int) []ChatMessage {
	out := l.TrimHistoryToBudget(history, budget)
	if dropped := len(history) - len(out); dropped > 0 {
		reportContextNotice(ctx, ContextNotice{Trimmed: true, DroppedMessages: dropped})
	}
	return out
}
