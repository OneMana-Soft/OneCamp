package business

// agentAmbient.go — the pure candidacy core for ambient mode.
//
// Ambient lets an opted-in agent reply in its scoped channels without an
// @mention. Running the model on EVERY channel message would be costly and
// spammy, so a cheap, deterministic pre-filter decides which messages are even
// worth considering; a per-(agent,channel) cooldown then rate-limits, and the
// agent itself self-selects (stays silent unless useful). This file is just the
// pure pre-filter + keyword parsing so every case is unit-testable without a DB
// or the event bus.

import "strings"

// maxAmbientKeywords caps how many topic keywords one agent tracks, so a huge
// list can't slow the per-message check.
const maxAmbientKeywords = 40

// parseAmbientKeywords splits an agent's raw ambient_keywords (comma or newline
// separated) into a normalized, de-duplicated, lower-cased list. Total + pure.
func parseAmbientKeywords(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		k := strings.ToLower(strings.TrimSpace(f))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
		if len(out) >= maxAmbientKeywords {
			break
		}
	}
	return out
}

// ambientCandidate reports whether a channel message is worth an ambient
// agent's consideration: it either asks a question (contains '?') or mentions
// one of the agent's topic keywords. With no keywords configured, only
// questions qualify — the most conservative default (agents don't chime in on
// every statement). Blank/whitespace messages never qualify. Pure + total.
func ambientCandidate(text string, keywords []string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.Contains(t, "?") {
		return true
	}
	if len(keywords) == 0 {
		return false
	}
	lower := strings.ToLower(t)
	for _, k := range keywords {
		if k != "" && strings.Contains(lower, k) {
			return true
		}
	}
	return false
}
