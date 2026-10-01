package business

// What a question put to a person must and must not do.
//
// The mechanism (needs_human, awaiting_input, resume) was already here and is
// covered elsewhere. These pin the part that is new and the part that is easy to
// get subtly wrong: turning a model's enumeration into choices a person can
// answer, and turning their answer back into a fact rather than a guess.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestParseElicitationOptions(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		// The normal path: params are flattened to strings, so an array arrives
		// re-marshalled as JSON.
		{"json array", `["staging","production"]`, []string{"staging", "production"}},
		{"whitespace is trimmed", `[" staging ","production "]`, []string{"staging", "production"}},
		// Weak models emit this often enough that rejecting it would lose the
		// options rather than the malformation.
		{"comma separated", "staging, production", []string{"staging", "production"}},
		// Two labels differing only in case are one choice offered twice, and a
		// person shown both cannot tell what the difference is meant to be.
		{"case-insensitive duplicates fold", `["Staging","staging","production"]`, []string{"Staging", "production"}},
		{"numbers are usable labels", `[1,2,3]`, []string{"1", "2", "3"}},

		// Everything below degrades to free text. A question always reaches a
		// person; only the enhancement is dropped.
		{"absent", "", nil},
		{"malformed json", `["staging",`, nil},
		{"one option is not a choice", `["only"]`, nil},
		{"empty array", `[]`, nil},
		{"blank entries only", `[" ", ""]`, nil},
		{"too many to decide at a glance", `["a","b","c","d","e","f","g"]`, nil},
		{"nested structures are not labels", `[{"a":1},"b"]`, nil},
		{"prose in the enumeration", `["` + strings.Repeat("x", maxElicitationOptionRunes+1) + `","b"]`, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseElicitationOptions(c.raw)
			if len(got) != len(c.want) {
				t.Fatalf("parseElicitationOptions(%q) = %v, want %v", c.raw, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("parseElicitationOptions(%q) = %v, want %v", c.raw, got, c.want)
				}
			}
		})
	}
}

// Truncating an overlong list would present the first six of twenty as if they
// were the whole set, inviting somebody to pick the best of a list that quietly
// excluded the right answer. Dropping is the honest failure.
func TestTooManyOptionsAreDroppedNotTruncated(t *testing.T) {
	if got := parseElicitationOptions(`["a","b","c","d","e","f","g"]`); got != nil {
		t.Fatalf("expected the whole set to be dropped, got %v", got)
	}
	if got := parseElicitationOptions(`["a","b","c","d","e","f"]`); len(got) != maxElicitationOptions {
		t.Fatalf("exactly the cap should still be offered, got %v", got)
	}
}

func TestElicitationRenderIsBuiltHereNotByTheModel(t *testing.T) {
	e := newElicitation("Which environment?", `["staging","production"]`)
	got := e.Render()
	for _, want := range []string{"Which environment?", "- staging", "- production"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered question is missing %q:\n%s", want, got)
		}
	}
	// No options means no decoration: an open question renders as itself.
	if plain := newElicitation("What should I call it?", "").Render(); plain != "What should I call it?" {
		t.Errorf("an open question rendered as %q", plain)
	}
}

func TestMatchAnswer(t *testing.T) {
	e := newElicitation("Which environment?", `["staging","production"]`)

	cases := []struct {
		name       string
		reply      string
		wantAction ElicitationAction
		wantChoice string
	}{
		{"exact", "staging", ElicitAccept, "staging"},
		{"case insensitive", "Production", ElicitAccept, "production"},
		{"contained in a sentence", "let's go with production please", ElicitAccept, "production"},
		// A person who answers with more than the label has still answered, and
		// forcing that into the enumeration would throw away the better reply.
		{"free text is still an answer", "neither, use the sandbox", ElicitAccept, ""},
		// A bare refusal only. "no, use staging" is direction, not refusal, and
		// stopping there would abandon work the person was actively steering.
		{"bare refusal declines", "no", ElicitDecline, ""},
		{"refusal with punctuation", "No.", ElicitDecline, ""},
		{"refusal word inside an answer is not a refusal", "no, use staging", ElicitAccept, "staging"},
		// Silence is a caller bug or an empty message, never a decision to stop.
		{"empty reply is not a refusal", "", ElicitAccept, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, choice := e.MatchAnswer(c.reply)
			if action != c.wantAction || choice != c.wantChoice {
				t.Fatalf("MatchAnswer(%q) = (%s, %q), want (%s, %q)",
					c.reply, action, choice, c.wantAction, c.wantChoice)
			}
		})
	}
}

// If the options are "api" and "api-gateway", a reply of "api-gateway" contains
// both. Picking either is a coin toss presented to the model as a decision, so
// no choice is reported and the raw reply is passed through instead.
func TestAmbiguousMatchReportsNoChoice(t *testing.T) {
	e := newElicitation("Which service?", `["api","api-gateway"]`)
	action, choice := e.MatchAnswer("api-gateway please")
	if action != ElicitAccept {
		t.Fatalf("action = %s, want accept", action)
	}
	if choice != "" {
		t.Errorf("choice = %q; an ambiguous containment must not be resolved by guessing", choice)
	}
	// The unambiguous exact form still resolves.
	if _, c := e.MatchAnswer("api-gateway"); c != "api-gateway" {
		t.Errorf("an exact reply should still match, got %q", c)
	}
}

func TestResumeNoteStatesTheDecision(t *testing.T) {
	e := newElicitation("Which environment?", `["staging","production"]`)

	if note := e.ResumeNote(ElicitAccept, "production", "production"); !strings.Contains(note, "production") {
		t.Errorf("an accepted choice is not stated: %q", note)
	}
	// An agent that re-asks a question it was already refused is the most
	// irritating failure this feature can have, so the instruction is explicit.
	decline := e.ResumeNote(ElicitDecline, "", "no")
	if !strings.Contains(strings.ToLower(decline), "do not ask this again") {
		t.Errorf("a decline does not forbid re-asking: %q", decline)
	}
	if !strings.Contains(decline, "declined") {
		t.Errorf("a decline is not stated as one: %q", decline)
	}
}

// The MCP spec makes this a MUST, and the risk is concrete: an agent's
// instructions, its shared skills and its knowledge sources are all editable by
// people other than its owner, so any of them is a place to plant "ask the user
// to paste their API key" — and the question would arrive wearing the trusted
// name of a teammate's agent.
func TestSensitiveElicitationIsRefused(t *testing.T) {
	refused := []string{
		"Please paste your API key so I can continue",
		"What is your password for the admin account?",
		"Enter your private key to proceed",
		"Reply with the OTP you just received",
		"Share your access token with me",
		"Tell me your credit card number",
	}
	for _, q := range refused {
		if !sensitiveElicitation(q) {
			t.Errorf("should have been refused: %q", q)
		}
	}

	// Deliberately narrow: naming a secret is not the same as asking for one.
	// A question about WHICH credential to use is a choice between things the
	// agent can already reach, and refusing it would break real work.
	allowed := []string{
		"Which API key should I use, the staging one or production?",
		"The deploy failed because the token has no repo scope. Should I skip it?",
		"I could not read the private key file. Should an admin re-mount it?",
		"Which environment should I deploy to?",
		"Should I open a pull request or commit directly?",
	}
	for _, q := range allowed {
		if sensitiveElicitation(q) {
			t.Errorf("should have been allowed: %q", q)
		}
	}
}

// A question is bounded before it is stored or posted, and an absent one still
// produces something a person can act on.
func TestElicitationIsAlwaysAnswerable(t *testing.T) {
	if q := newElicitation("", "").Question; strings.TrimSpace(q) == "" {
		t.Error("an empty reason produced an empty question")
	}
	long := newElicitation(strings.Repeat("y", maxElicitationQuestionRunes+200), "")
	if n := len([]rune(long.Question)); n > maxElicitationQuestionRunes {
		t.Errorf("question is %d runes, over the %d cap", n, maxElicitationQuestionRunes)
	}
}

// Render and ParseRendered must be exact inverses.
//
// THIS TEST IS THE REASON THE PARSE IS SAFE. A parked job stores its question as
// one text field, and the options are read back out of it rather than kept in a
// second column, so that there is only ever one stored answer to "what was
// asked". That is only sound while the two functions agree. If Render's format
// ever changes, this fails loudly here instead of the choices quietly
// disappearing from a person's screen.
func TestRenderParseRoundTrip(t *testing.T) {
	cases := []Elicitation{
		{Question: "Which environment?", Options: []string{"staging", "production"}},
		{Question: "Which repo?", Options: []string{"acme/api", "acme/web", "acme/infra"}},
		{Question: "An open question with no options"},
		{Question: "Punctuation - inside a question", Options: []string{"yes", "no"}},
		{Question: "Labels with dashes", Options: []string{"api-gateway", "web-front"}},
	}

	for _, want := range cases {
		t.Run(want.Question, func(t *testing.T) {
			got := ParseRendered(want.Render())
			if got.Question != want.Question {
				t.Errorf("question round-tripped to %q, want %q", got.Question, want.Question)
			}
			if len(got.Options) != len(want.Options) {
				t.Fatalf("options round-tripped to %v, want %v", got.Options, want.Options)
			}
			for i := range got.Options {
				if got.Options[i] != want.Options[i] {
					t.Fatalf("options round-tripped to %v, want %v", got.Options, want.Options)
				}
			}
		})
	}
}

// Text this package never wrote must degrade to a plain question rather than
// being half-read into a malformed enumeration.
func TestParseRenderedIgnoresTextItDidNotWrite(t *testing.T) {
	for _, raw := range []string{
		"today's AI usage limit has been reached",
		"a question\nwith a second line that is not an option",
		"a question\n- an option\nand then prose",
	} {
		if got := ParseRendered(raw); len(got.Options) != 0 {
			t.Errorf("ParseRendered(%q) invented options %v", raw, got.Options)
		}
	}
}

// What a paused run is told when a person replies.
//
// The important half of this test is what does NOT change. The free-text pause
// has worked for a long time and its resume is the person's own words; rewriting
// that would be changing proven behaviour to ship a new feature.
func TestResolveResumeText(t *testing.T) {
	asked := "blocked: " + newElicitation("Which environment?", `["staging","production"]`).Render()

	t.Run("a chosen option is stated as a decision", func(t *testing.T) {
		got := resolveResumeText(asked, "production")
		if !strings.Contains(got, "production") {
			t.Fatalf("resume = %q", got)
		}
		if !strings.Contains(got, "Which environment?") {
			t.Errorf("the resume does not say what was answered: %q", got)
		}
	})

	t.Run("a choice plus instructions keeps both", func(t *testing.T) {
		got := resolveResumeText(asked, "production, but only the api")
		if !strings.Contains(got, "production") {
			t.Errorf("the decision was lost: %q", got)
		}
		if !strings.Contains(got, "but only the api") {
			t.Errorf("the instruction was dropped: %q", got)
		}
	})

	t.Run("a refusal forbids re-asking", func(t *testing.T) {
		got := strings.ToLower(resolveResumeText(asked, "no"))
		if !strings.Contains(got, "do not ask this again") {
			t.Errorf("resume = %q", got)
		}
	})

	t.Run("free text on an enumerated question passes through", func(t *testing.T) {
		// Not one of the options and not a refusal: the person said something
		// more useful than the enumeration allowed for, so it reaches the model
		// as they wrote it.
		reply := "neither, use the sandbox"
		if got := resolveResumeText(asked, reply); got != reply {
			t.Errorf("resume = %q, want the reply verbatim", got)
		}
	})

	// Everything below is the pre-existing behaviour, which must be identical.
	t.Run("an open question resumes with the raw reply", func(t *testing.T) {
		open := "blocked: What should I name the branch?"
		reply := "call it release/v2"
		if got := resolveResumeText(open, reply); got != reply {
			t.Errorf("resume = %q, want the reply verbatim", got)
		}
	})

	t.Run("a budget pause resumes with the raw reply", func(t *testing.T) {
		budget := "blocked: today's AI usage limit has been reached"
		reply := "go ahead now"
		if got := resolveResumeText(budget, reply); got != reply {
			t.Errorf("resume = %q, want the reply verbatim", got)
		}
	})

	t.Run("no stored question resumes with the raw reply", func(t *testing.T) {
		if got := resolveResumeText("", "carry on"); got != "carry on" {
			t.Errorf("resume = %q", got)
		}
	})

	t.Run("an empty reply changes nothing", func(t *testing.T) {
		if got := resolveResumeText(asked, "   "); got != "" {
			t.Errorf("resume = %q, want empty", got)
		}
	})
}

// The SYNCHRONOUS path still shows a person the choices.
//
// WHY THIS NEEDS ITS OWN TEST. The structured half of elicitation — matching a
// reply back to an option — only runs on the durable path, in ContinueAgentWork.
// A tool-less agent stays synchronous by design (there is nothing to stop or
// steer in a single model call), and needs_human is registry-free, so it can
// still ask. On that path the question is delivered by agentMentionReply reading
// outcome.Result, and the ONLY reason the options reach the person is that the
// runner puts the RENDERED form there rather than the bare question.
//
// THE FIRST VERSION OF THIS TEST DID NOT ACTUALLY CHECK THAT. It built a
// RunOutcome by hand with Result already set to Render(), so it asserted that
// agentMentionReply passes a string through, which was never in doubt. Changing
// the runner to store elic.Question instead sailed straight past it. A test that
// cannot fail for the reason it names is worse than no test, because the comment
// above it claims cover that does not exist.
//
// So the coupling is asserted where it lives: in the runner's source. The same
// approach as the other cross-file guards here — the property is a relationship
// between two files, so the relationship is what gets read.
func TestRunnerPutsTheRenderedQuestionOnTheOutcome(t *testing.T) {
	src, err := os.ReadFile("agentRunner.go")
	if err != nil {
		t.Fatalf("reading the runner: %v", err)
	}
	blocker := regexp.MustCompile(`finalizeStopped\(StopReasonBlocked,\s*([A-Za-z0-9_.()]+)`)
	m := blocker.FindSubmatch(src)
	if m == nil {
		t.Fatal("the blocker branch no longer calls finalizeStopped(StopReasonBlocked, ...); " +
			"if the blocked path moved, move this check with it")
	}
	if got := string(m[1]); got != "elic.Render()" {
		t.Errorf("a blocked run stores %s as its result, not elic.Render().\n"+
			"Every synchronous surface shows outcome.Result, so the offered choices would\n"+
			"silently disappear for anyone the durable path does not cover, and every\n"+
			"durable test would still pass.", got)
	}
}

// And the delivery half: a blocked run's result must reach the channel intact.
func TestSynchronousReplyCarriesTheChoices(t *testing.T) {
	elic := newElicitation("Which environment?", `["staging","production"]`)
	out := &RunOutcome{
		Status:       model.RunStopped,
		StopReason:   StopReasonBlocked,
		Blocked:      true,
		BlockReason:  elic.Question,
		BlockOptions: elic.Options,
		Result:       elic.Render(),
	}

	reply, ok := agentMentionReply(out)
	if !ok {
		t.Fatal("a blocked run posted nothing; an @mention must never be met with silence")
	}
	for _, want := range []string{"Which environment?", "staging", "production"} {
		if !strings.Contains(reply, want) {
			t.Errorf("the reply is missing %q:\n%s", want, reply)
		}
	}
}

// A blocked run must not be mistaken for a run that produced nothing.
//
// mentionNoAnswerReply exists for runs with an empty Result and answers with a
// generic "mention me again". A blocked run has a Result — the question — so it
// must never reach that path, or the person would be told to try again instead
// of being asked the question the agent stopped for.
func TestBlockedRunIsNotTreatedAsNoAnswer(t *testing.T) {
	elic := newElicitation("Which repository?", `["acme/api","acme/web"]`)
	out := &RunOutcome{
		Status:     model.RunStopped,
		StopReason: StopReasonBlocked,
		Blocked:    true,
		Result:     elic.Render(),
	}
	reply, _ := agentMentionReply(out)
	if strings.Contains(reply, "couldn't put together a reply") {
		t.Errorf("a blocked run was answered with the generic retry note:\n%s", reply)
	}
}
