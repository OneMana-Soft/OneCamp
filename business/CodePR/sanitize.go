package codepr

// sanitize.go — scrubbing anything that leaves the sandbox before it is shown to a
// user, written into the audit ledger, or published to GitHub in a pull-request
// body. This is the single choke point: runner messages, verifier gate summaries,
// diff excerpts and error strings all pass through Sanitize.
//
// It is deliberately small. This file replaces a 290-line "verifier layer" that
// also contained a build-system detector, a gate planner and an output summarizer.
// None of those had a single production caller: the runner detects build systems
// and summarizes output itself, in the process that actually executes the gates,
// and there was no wire field to carry a server-authored plan to it. Two
// implementations of the same job, only one of them running, meant the unused copy
// silently drifted — its Go plan gated on `gofmt -l .`, which lists unformatted
// files and exits ZERO, so it would have passed unconditionally, and its path
// redaction never matched a real checkout path. Keeping a worse duplicate around
// for a capability the runner already provides is a liability, so it is gone; what
// remains is the part with real consumers.

import "regexp"

// runRootRe matches the runner's EPHEMERAL run directory together with the
// checkout or scratch directory inside it, so a build message keeps only its
// repo-relative tail (`main.go:12: …`) instead of disclosing the sandbox's
// filesystem layout in a published pull-request body.
//
// This shape is matched explicitly because it is the one that actually occurs: the
// runner creates its per-run root with MkdirTemp(workRoot(), "coderun-"), giving
// `/work/coderun-<random>/repo/…`. absPathRe below expects the run directory to be
// a hex-ish name and therefore never matched a real path — it was written against
// an imagined layout and, being untested, silently redacted nothing for the entire
// life of the feature.
var runRootRe = regexp.MustCompile(`(?i)/?(?:[^\s:]*/)?coderun-[^/\s:]+/(?:repo|verifier-tmp|checkout)?/?`)

// absPathRe matches other absolute host paths with an opaque id segment so they
// can be stripped to a repo-relative-ish tail. Kept alongside runRootRe to cover
// checkout roots created by a different convention (an operator-mounted work dir,
// a future runner layout) rather than only today's.
var absPathRe = regexp.MustCompile(`(/[^\s:]*/)?(work|checkout|src|repo|tmp)/[0-9a-f-]{8,}/`)

// tokenRe matches long token-ish secrets (GitHub PATs, generic bearer tokens) so
// they can never appear in a summary shown to a user or stored in audit.
var tokenRe = regexp.MustCompile(`(?i)(gh[posru]_[A-Za-z0-9]{20,}|x-access-token:[^@\s]+|bearer\s+[A-Za-z0-9._-]{20,}|[A-Za-z0-9._-]{40,})`)

// Sanitize strips host absolute paths and token-looking substrings from text
// before it is shown to a user, recorded, or published. Pure; safe to call on any
// runner output, error message, verifier summary, or diff excerpt.
func Sanitize(s string) string {
	s = runRootRe.ReplaceAllString(s, "")
	s = absPathRe.ReplaceAllString(s, "")
	s = tokenRe.ReplaceAllString(s, "[redacted]")
	return s
}
