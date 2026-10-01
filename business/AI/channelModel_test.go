package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// llmCallPatterns are the ways this package reaches a model. A channel-scoped function
// that matches any of them has to have chosen its client deliberately.
var llmCallPatterns = []string{
	".Chat(",
	".ChatStream(",
	"Summarize(",
	"SummarizeWith(",
	"streamChatAnswer(",
	"streamChatAnswerRaw(",
}

// topLevelFuncs splits a Go source file into (signature, body) pairs for its top-level
// functions. Crude but sufficient: this package writes one func per top-level `func` at
// column zero, and a body runs until the next one.
func topLevelFuncs(src string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(src, "\n")
	sig, body := "", []string{}
	flush := func() {
		if sig != "" {
			out[sig] = strings.Join(body, "\n")
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "func ") {
			flush()
			sig, body = line, nil
			continue
		}
		if sig != "" {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// THE RATCHET, and it is deliberately derived rather than a list of known functions.
//
// An admin who pins a model to a channel means it for the AI work that happens in that
// channel. Before ChannelScopedLLM existed, exactly one caller honoured the pin — the
// agent runner — and three others silently used the workspace default: the channel
// summary and the @mention answer in both its streaming and non-streaming forms. From
// the admin's side the setting is named for the channel and looks like it applies to the
// channel, so the exceptions were undiscoverable.
//
// Checking a hardcoded list would not have prevented that and would not prevent the
// next one. So this finds every function in the package that TAKES a channel and REACHES
// a model, and requires it to have resolved a channel-scoped client. A new channel
// feature is covered the moment it is written.
func TestEveryChannelScopedAICallHonoursThePinnedModel(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	checked := 0
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		// Comments stripped so prose mentioning a call cannot satisfy or trip the rule.
		src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

		for sig, body := range topLevelFuncs(src) {
			// Channel-scoped means the function is handed a channel to act on.
			if !strings.Contains(sig, "channelUUID") {
				continue
			}
			callsModel := false
			for _, p := range llmCallPatterns {
				if strings.Contains(body, p) {
					callsModel = true
					break
				}
			}
			if !callsModel {
				continue // reads, formats, stores — nothing to choose a model for
			}
			checked++

			if !strings.Contains(body, "ChannelScopedLLM(") {
				fn := sig
				if i := strings.Index(fn, "("); i > 0 {
					fn = fn[:i]
				}
				t.Errorf("%s in %s reaches a model but never resolves the channel's pinned "+
					"model.\n  -> call ChannelScopedLLM(ctx, channelUUID) and use the client "+
					"it returns.\n  Otherwise an admin's per-channel model applies to agent "+
					"runs and silently not to this, which makes the setting unreliable for "+
					"both cost control and residency.", fn, name)
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no channel-scoped functions that call a model; this ratchet is " +
			"passing vacuously and needs its detection updated")
	}
	t.Logf("verified %d channel-scoped AI call site(s) honour the pinned model", checked)
}

// The resolver must always hand back a usable client. Every caller uses the result
// directly — including for RecordResult / RecordSuccess on the breaker — so returning a
// nil client on an unresolvable channel would turn an admin's stale pin into a panic on
// a live request path.
func TestChannelScopedLLMNeverReturnsANilClientWhenAIIsAvailable(t *testing.T) {
	// Without an initialised service the honest answer IS nil, and callers reach this
	// only after their own IsEnabled check. Asserting the contract as documented.
	llm, cb, label, limits := ChannelScopedLLM(nil, "not-a-uuid")
	if llm != nil || cb != nil || label != "" {
		t.Fatalf("with no AI service the resolver must return nothing, got (%v, %v, %q)",
			llm, cb, label)
	}
	// The limits are the exception to "return nothing": a caller that budgets with them
	// must get a usable window even here, because a zero window would make every
	// truncation drop its whole input rather than fall back to the workspace default.
	if limits.ContextWindow <= 0 {
		t.Errorf("limits must always be usable, got a context window of %d", limits.ContextWindow)
	}
}

// The fallback ladder must be total: every unresolvable step yields the default rather
// than an error, because a channel preference about WHICH model must never become a veto
// on whether AI works at all.
func TestChannelScopedLLMFallsBackRatherThanFailing(t *testing.T) {
	raw, err := os.ReadFile("channelModel.go")
	if err != nil {
		t.Fatalf("read channelModel.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	// Each guarded step must return the default pair, never an error value.
	if strings.Contains(src, "return nil, nil, err") || strings.Contains(src, ", error)") {
		t.Error("ChannelScopedLLM returns an error. A channel's model preference must " +
			"degrade to the workspace default, never fail the call — summarising with the " +
			"default model is always better than refusing to summarise")
	}
	for _, step := range []string{
		"uuid.Parse(",           // unparseable channel id
		"GetChannelAIModel(",    // no pin, or lookup failure
		"GetAuthorizedModel(",   // pin points at a removed allowlist entry
		"Usable()",              // pin points at a disabled model
		"ResolveExplicitModel(", // residency or provider refusal
	} {
		if !strings.Contains(src, step) {
			t.Errorf("the fallback ladder no longer covers %s; an unresolvable pin at that "+
				"step would not degrade to the workspace default", step)
		}
	}
}
