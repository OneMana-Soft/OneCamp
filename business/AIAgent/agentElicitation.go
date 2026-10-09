package business

// Structured elicitation: an agent that asks a person a question can also offer
// the answers.
//
// WHAT ALREADY EXISTED. The needs_human blocker has been here for a while and it
// works: an agent that cannot proceed stops cleanly, the question reaches a
// person, and a durable run parks as awaiting_input and resumes on the reply.
// The mechanism is not the gap.
//
// THE GAP WAS SHAPE. The question was one free-text string, so an agent asking
// "which repository?" wrote the candidates into its own prose and the person
// typed something back that the model then had to re-interpret. The codebase
// already knew this was unsatisfying — agentGithubContext.go instructs the model
// to "list the candidate owner/name options in your question", which is exactly
// an enumeration being smuggled through prose because there was nowhere to put
// it. Re-interpreting a free-text reply is a guess, and this runner carries
// verifyClaims and verifyWorkHappened precisely because a model's guess is not
// evidence.
//
// SO OPTIONS ARE FIRST-CLASS. The model may enumerate the answers; the person
// picks one; the run resumes knowing which. Nothing is inferred that was stated.
//
// THE SHAPE FOLLOWS MCP'S ELICITATION SPEC (2025-06-18), deliberately rather
// than inventing one:
//
//   - An enumerated choice is the spec's "Enum Schema" flattened to the one
//     field a chat reply can carry. Multi-field forms are omitted: a person
//     answering in a thread is not filling in a form.
//   - accept / decline / cancel is kept whole, because collapsing decline into
//     cancel is what makes an agent re-ask a question it was already refused.
//   - So is the rule that elicitation MUST NOT request sensitive information.
//     See sensitiveElicitation.
//
// Everything here is pure. No database, no model call, no clock.

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// maxElicitationOptions caps how many choices one question may offer.
	//
	// Six because the point of enumerating is that a person can decide at a
	// glance. A model that produces twenty candidates has not narrowed anything
	// down, and a list that long renders as a wall in a message and as an
	// unusable row of buttons on a phone. Past the cap the options are dropped
	// rather than truncated: showing the first six of twenty invites somebody to
	// pick the best of a set that quietly excluded the right answer.
	maxElicitationOptions = 6
	// maxElicitationOptionRunes caps one option's length. An option is a label,
	// not a sentence; anything longer is prose that belongs in the question.
	maxElicitationOptionRunes = 80
	// maxElicitationQuestionRunes bounds the question itself, which is rendered
	// into a channel message and stored on the task row.
	maxElicitationQuestionRunes = 500
)

// ElicitationAction is the outcome of putting a question to a person, using the
// MCP three-action vocabulary.
type ElicitationAction string

const (
	// ElicitAccept: the person answered. Choice carries what they chose.
	ElicitAccept ElicitationAction = "accept"
	// ElicitDecline: the person explicitly refused to answer. Distinct from
	// cancel: they are still here and still want the work, they are just not
	// answering this. An agent must not re-ask.
	ElicitDecline ElicitationAction = "decline"
	// ElicitCancel: the person dismissed the work entirely (they stopped the
	// job). Carried for completeness of the model; the stop path owns it.
	ElicitCancel ElicitationAction = "cancel"
)

// declineWords are the replies that mean "I am not answering this".
//
// Deliberately short and unambiguous. A long list of near-synonyms would start
// classifying real answers as refusals, and treating an answer as a decline is
// the expensive direction: the work stops when the person thought they had just
// told it what to do. Anything not listed is an answer.
var declineWords = map[string]bool{
	"decline": true, "no": true, "nope": true, "cancel": true, "skip": true,
	"stop": true, "none": true, "nevermind": true, "never mind": true,
	"n/a": true, "na": true, "don't": true, "dont": true,
}

// parseElicitationOptions reads the options an agent offered.
//
// The value arrives as a JSON array in a string, because a tool call's params
// are flattened to map[string]string by ToolCallToAction and a non-scalar is
// re-marshalled on the way through. A comma-separated string is also accepted:
// weaker models emit one often enough that rejecting it would mean losing the
// options rather than the malformation.
//
// EVERY FAILURE DEGRADES TO NO OPTIONS, never to an error. The question itself
// is always valid and always reaches a person; options are an enhancement, and
// a malformed enhancement must not turn a working pause into a failed run.
func parseElicitationOptions(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var parts []string
	if strings.HasPrefix(raw, "[") {
		// The normal path: a JSON array, possibly of non-strings if the model
		// was careless. Numbers and booleans are rendered rather than dropped,
		// because "1" is a perfectly good choice label.
		var anys []interface{}
		if err := json.Unmarshal([]byte(raw), &anys); err != nil {
			return nil
		}
		for _, a := range anys {
			switch v := a.(type) {
			case string:
				parts = append(parts, v)
			case float64, bool:
				parts = append(parts, strings.TrimSpace(strings.Trim(jsonScalar(v), `"`)))
			default:
				// An object or a nested array is not a label. Drop the whole
				// set: a partial list is the one outcome worse than none,
				// because it looks complete.
				return nil
			}
		}
	} else {
		parts = strings.Split(raw, ",")
	}

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len([]rune(p)) > maxElicitationOptionRunes {
			// One overlong label means the model is writing prose into the
			// enumeration. Truncating would present a mangled choice as a real
			// one, so the whole set goes.
			return nil
		}
		out = append(out, p)
	}

	out = dedupeFold(out)
	// One option is not a choice, it is a leading question. Two is the minimum
	// that gives a person something to decide.
	if len(out) < 2 || len(out) > maxElicitationOptions {
		return nil
	}
	return out
}

// jsonScalar renders a non-string JSON scalar the way stringifyArg would, so a
// numeric option reads the same here as everywhere else in the tool path.
func jsonScalar(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// dedupeFold removes case-insensitive duplicates, keeping first occurrence and
// its original casing.
//
// Case-insensitive because "Production" and "production" are one choice offered
// twice, and a person shown both cannot tell what the difference is meant to be.
// uniqueStrings in agentLearning.go folds case-sensitively, which is right for
// tool names and wrong for human-facing labels, so this is a second function
// rather than a change to that one.
func dedupeFold(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// sensitiveElicitation reports whether a question is fishing for a credential.
//
// THE SPEC MAKES THIS A MUST, not a nicety: "Servers MUST NOT use elicitation to
// request sensitive information." The risk here is concrete and not theoretical.
// An agent's instructions are editable, its skills are composed from a shared
// library, and its knowledge is pulled from workspace content that other people
// write. Any of those is a place to plant "ask the user to paste their API key",
// and the question would arrive wearing the trusted name of a teammate's agent.
//
// So this refuses at the point of asking. It matches the ASK, which is the thing
// under our control, and it is deliberately narrow: it fires on a request for a
// secret, not on the mere mention of one. "Which API key should I use?" names a
// choice between things the agent can already reach; "paste your API key" is an
// exfiltration. Only the second is refused.
func sensitiveElicitation(question string) bool {
	q := strings.ToLower(question)
	secret := []string{
		"password", "passwd", "api key", "api-key", "apikey", "secret key",
		"access token", "auth token", "bearer token", "private key",
		"credit card", "card number", "cvv", "ssn", "social security",
		"otp", "one-time code", "2fa code", "mfa code", "seed phrase",
		"recovery phrase",
	}
	hasSecret := false
	for _, s := range secret {
		if strings.Contains(q, s) {
			hasSecret = true
			break
		}
	}
	if !hasSecret {
		return false
	}
	// A verb that asks the person to HAND OVER the secret. Without one of these
	// the sentence is about a secret rather than a request for it.
	for _, v := range []string{
		"paste", "enter", "provide", "give me", "send me", "share", "type",
		"tell me", "what is your", "what's your", "confirm your", "reply with",
	} {
		if strings.Contains(q, v) {
			return true
		}
	}
	return false
}

// sensitiveElicitationRefusal is what the agent is told instead of pausing. It
// names the rule so the model corrects course rather than rephrasing the same
// request, which is what a bare "denied" reliably produces.
const sensitiveElicitationRefusal = "Refused: an agent must never ask a person for a password, key, token or " +
	"other credential. Use a connected account or an admin-registered server instead, or explain what access is " +
	"missing and let an admin configure it. Do not ask for this again."

// internalIDElicitation reports whether a question asks a person to supply an
// internal identifier: a UUID, or an id the product assigns. People do not
// know these and should never be asked; the demo agent, holding a tool that
// takes a project id and none that finds one, asked a visitor for "the UUID of
// the Q4 launch project". Pure.
func internalIDElicitation(question string) bool {
	q := strings.ToLower(question)
	if strings.Contains(q, "uuid") || strings.Contains(q, "guid") {
		return true
	}
	if !idWord.MatchString(q) {
		return false
	}
	for _, v := range []string{
		"provide", "give me", "send me", "share", "tell me", "paste", "enter",
		"what is the", "what's the", "reply with", "need the",
	} {
		if strings.Contains(q, v) {
			return true
		}
	}
	return false
}

var idWord = regexp.MustCompile(`\b(id|ids|identifier|identifiers)\b`)

// internalIDElicitationRefusal is what the agent is told instead of pausing.
const internalIDElicitationRefusal = "Refused: people do not know internal ids, so never ask for one. Find it " +
	"yourself: list or search with the tools you have and match by name (for example, list the projects and pick " +
	"the one named in the question). If none of your tools can find it, answer with what you can and say which " +
	"tool would let you do the rest."

// Elicitation is one question put to a person, with the answers it offers.
type Elicitation struct {
	// Question is what the agent needs to know, already bounded.
	Question string `json:"question"`
	// Options are the answers offered. Empty means the reply is free text.
	Options []string `json:"options,omitempty"`
}

// newElicitation builds a bounded, valid question from a raw tool call.
func newElicitation(reason, rawOptions string) Elicitation {
	q := strings.TrimSpace(reason)
	if q == "" {
		q = "I need input from a person to continue."
	}
	return Elicitation{
		Question: helpers.TruncateRunes(q, maxElicitationQuestionRunes),
		Options:  parseElicitationOptions(rawOptions),
	}
}

// Render turns a question into the text a person reads.
//
// BUILT HERE RATHER THAN BY THE MODEL, so the options a person sees are the same
// list the resume matches against. A model asked to restate its own enumeration
// in prose eventually restates it differently.
func (e Elicitation) Render() string {
	if len(e.Options) == 0 {
		return e.Question
	}
	var b strings.Builder
	b.WriteString(e.Question)
	for _, o := range e.Options {
		b.WriteString("\n- ")
		b.WriteString(o)
	}
	return b.String()
}

// ParseRendered is the exact inverse of Render.
//
// WHY AN INVERSE RATHER THAN A COLUMN. A parked job stores what it asked in one
// text field, and that same text is posted into the thread, so it has to stay
// readable by a person. Adding a parallel JSON column for the options would give
// two stored representations of one question and a way for them to disagree,
// which is the failure this file exists to avoid everywhere else.
//
// The parse is safe because BOTH sides are ours: Render writes "\n- " before
// each option and nothing else in this package does. TestRenderParseRoundTrip
// pins that; if Render's format ever changes, that test fails rather than the
// options quietly vanishing from a person's screen.
//
// Anything that does not look like a rendered question comes back as a plain
// question with no options, so a note written by an older build, or by the
// budget-pause path that shares this field, degrades to exactly what it is.
func ParseRendered(s string) Elicitation {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	e := Elicitation{Question: strings.TrimSpace(lines[0])}
	for _, l := range lines[1:] {
		l = strings.TrimRight(l, " \t")
		if !strings.HasPrefix(l, "- ") {
			// A body line that is not an option means this was never a rendered
			// enumeration. Return the whole thing as the question rather than a
			// half-read one.
			return Elicitation{Question: strings.TrimSpace(s)}
		}
		if o := strings.TrimSpace(strings.TrimPrefix(l, "- ")); o != "" {
			e.Options = append(e.Options, o)
		}
	}
	return e
}

// MatchAnswer classifies a person's reply against what was asked.
//
// Returns the action and, for an accept, the option that was chosen. A reply
// that is not a listed option is still an accept: a person who answers "the
// staging one, but only the api" has answered, and forcing that into an
// enumeration would throw away the more useful reply. The choice is only
// reported when it is unambiguous, so a caller can always tell "they picked
// option 2" from "they said something".
func (e Elicitation) MatchAnswer(reply string) (ElicitationAction, string) {
	r := strings.TrimSpace(reply)
	if r == "" {
		// Nothing was said. Not a refusal — a resume with an empty follow-up is
		// a caller bug or an empty message, and treating silence as "no" would
		// abandon work nobody declined.
		return ElicitAccept, ""
	}

	// A bare refusal, and only a bare one. "no" is a decline; "no, use staging"
	// is an answer that begins with the word no, and misreading it as a refusal
	// would stop work the person was actively directing.
	if declineWords[strings.ToLower(strings.Trim(r, " .!"))] {
		return ElicitDecline, ""
	}

	if len(e.Options) == 0 {
		return ElicitAccept, ""
	}

	// Exact match first, so an option that is a substring of another cannot be
	// stolen by the longer one.
	for _, o := range e.Options {
		if strings.EqualFold(r, o) {
			return ElicitAccept, o
		}
	}
	// Then a contained match, but only when EXACTLY ONE option matches. If the
	// options are "api" and "api-gateway", a reply of "api-gateway" contains
	// both, and picking either is a coin toss presented as a decision.
	var hit string
	n := 0
	lower := strings.ToLower(r)
	for _, o := range e.Options {
		if strings.Contains(lower, strings.ToLower(o)) {
			hit = o
			n++
		}
	}
	if n == 1 {
		return ElicitAccept, hit
	}
	return ElicitAccept, ""
}

// declinedNoteLead starts the note a declined question resumes with, which
// splitAskerWords leaves out of the asker's words (withoutDeclines).
const declinedNoteLead = "The person declined to answer \""

// answeredNoteLead and repliedNoteLead start the notes for an option chosen
// and for a free reply to a question (withoutDeclines reads them).
const (
	answeredNoteLead = "The person answered \""
	repliedNoteLead  = "The person replied to \""
)

// ResumeNote is what the agent is told when the run picks back up.
//
// The chosen option is stated as fact when there was one, so the model resumes
// from a decision rather than re-parsing the sentence it already caused to be
// written. A decline is stated as a decline, with the instruction not to re-ask,
// because an agent that asks the same question twice after being refused is the
// single most irritating failure mode this whole feature can have.
func (e Elicitation) ResumeNote(action ElicitationAction, choice, reply string) string {
	switch action {
	case ElicitDecline:
		return declinedNoteLead + e.Question + "\". Do not ask this again. " +
			"Continue with what you can do without it, or stop and say plainly what is left undone."
	case ElicitCancel:
		return "The person cancelled this work."
	}
	if choice != "" {
		return answeredNoteLead + e.Question + "\" with: " + choice
	}
	if r := strings.TrimSpace(reply); r != "" {
		return repliedNoteLead + e.Question + "\": " + r
	}
	return ""
}

// resolveResumeText decides what a paused run is told when a person replies.
//
// CONSERVATIVE BY CONSTRUCTION. The free-text pause has worked for a long time
// and its resume is simply the person's words, so that path is returned
// unchanged. Only two cases rewrite it, and both are cases where the raw words
// are demonstrably not enough:
//
//   - The person picked one of the offered answers, so the decision is stated
//     rather than left to be re-parsed out of their sentence.
//   - The person refused, and the one behaviour that must not follow a refusal
//     is asking again.
//
// parked is the task's stored blocker text ("blocked: <rendered question>").
// Anything that is not a question this package rendered yields the reply
// untouched, so a budget pause, an older build's note, and an open question all
// behave exactly as they did before.
func resolveResumeText(parked, reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return reply
	}
	elic := ParseRendered(strings.TrimPrefix(strings.TrimSpace(parked), "blocked: "))
	if elic.Question == "" {
		return reply
	}

	action, choice := elic.MatchAnswer(reply)
	switch {
	case action == ElicitDecline:
		return elic.ResumeNote(action, "", reply)
	case choice != "":
		// The person's own words are kept alongside the resolved choice: they
		// may have said "production, but only the api", and dropping the rest
		// would answer the question while discarding the instruction.
		note := elic.ResumeNote(action, choice, reply)
		if !strings.EqualFold(strings.TrimSpace(reply), choice) {
			note += "\n\nTheir full reply: " + reply
		}
		return note
	}
	return reply
}

// blockerCallFromText recognises a needs_human call a model wrote as its reply
// instead of calling the tool.
//
// Smaller models do this: on the demo, gpt-oss-20b ended a delegated task with
// the text {"options":[...],"reason":"I need the UUID of the Launch sync notes
// document..."}, which was posted to the task verbatim and the job marked done.
// The shape is unmistakable (the tool's own arguments and nothing else, bare
// or wrapped as {"name":"needs_human","arguments":{...}}), so it becomes the
// call it meant to be, and passes through the same checks as a real one: an
// internal id is looked up, never put to a person. Anything else is left
// alone. Pure.
func blockerCallFromText(text string) (ai.ProposedAction, bool) {
	s := strings.TrimSpace(text)
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```"))
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return ai.ProposedAction{}, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) != nil {
		return ai.ProposedAction{}, false
	}
	if name, ok := obj["name"]; ok {
		var n string
		if json.Unmarshal(name, &n) != nil || n != blockerToolName {
			return ai.ProposedAction{}, false
		}
		args, ok := obj["arguments"]
		if !ok || len(obj) != 2 {
			return ai.ProposedAction{}, false
		}
		obj = nil
		if json.Unmarshal(args, &obj) != nil {
			return ai.ProposedAction{}, false
		}
	}
	var reason string
	if raw, ok := obj["reason"]; !ok || json.Unmarshal(raw, &reason) != nil || strings.TrimSpace(reason) == "" {
		return ai.ProposedAction{}, false
	}
	for k := range obj {
		if k != "reason" && k != "options" {
			return ai.ProposedAction{}, false
		}
	}
	params := map[string]string{"reason": strings.TrimSpace(reason)}
	if raw, ok := obj["options"]; ok {
		params["options"] = string(raw)
	}
	return ai.ProposedAction{ToolName: blockerToolName, Params: params}, true
}
