package codepr

// Repo resolution — deciding which linked repository a natural-language coding
// task refers to, deterministically and without ever a global search (mirrors
// agentGithubContext's discipline: the workspace already knows its repos; the
// agent must address one of them by exact owner/name, or ASK which).
//
// The matcher is PURE and fully unit-tested; the DB fetch is a thin wrapper.
// It handles the real cases a user phrases: an explicit "owner/name", a bare
// repo name (word-boundary matched so "onecamp" never matches inside
// "onecamp-fe"), the single-linked-repo shortcut, and genuine ambiguity /
// no-match (both surfaced as candidates for a single needs_human question).

import (
	"context"
	"regexp"
	"strings"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	"github.com/akashc777/OneCamp/helpers"
)

// RepoResolution is the outcome of resolving a task to a repo. Resolved is valid
// only when Found is true; otherwise Candidates lists the repos to offer the
// user in a needs_human question (empty when the workspace has no linked repos
// at all — a distinct, honest case).
type RepoResolution struct {
	Found      bool
	Resolved   RepoRef
	Candidates []RepoRef
	// NoneLinked is true when the workspace has NO linked repos, so the caller
	// asks the user to link one rather than to choose.
	NoneLinked bool
	// InaccessibleRepo is set when the user explicitly named a repo (owner/name)
	// that the connected GitHub account cannot reach, so the caller can say
	// exactly which repo it couldn't use instead of a generic "which repo?".
	InaccessibleRepo RepoRef
	// UnlinkedDisabled is set when the user EXPLICITLY named a repo that isn't
	// linked to a project while "work on any accessible repo" is turned OFF. The
	// caller then tells the user to link it (or enable the setting) rather than
	// silently working on a different linked repo.
	UnlinkedDisabled RepoRef
}

// RepoAccessChecker reports whether the ACTING identity can work on owner/name.
// Injected so repo resolution stays testable without a network call; the live path
// binds it to the run's own credential (see credentialRepoAccess in live.go), never
// to the workspace admin's connection.
type RepoAccessChecker func(ctx context.Context, owner, name string) (bool, error)

// ResolveRepo resolves the target repo for a coding task. It honors an
// EXPLICITLY-named "owner/name" (or github.com URL) first — so a repo the user
// actually named is never silently swapped for a different linked one — then
// falls back to matching the workspace's LINKED repositories (deterministic,
// never a global search). An explicitly-named repo that isn't linked is used
// only when allowUnlinked is on AND verify says the acting identity can reach it;
// when the feature is off, the named repo is surfaced as "not linked" rather than
// resolving something else. Best-effort DB fetch; on a lookup error it still tries
// an explicit repo before degrading to a clear needs_human.
//
// verify is a REQUIRED parameter rather than a package default, and it must be
// bound to the identity that will actually push. Resolution used to check with the
// workspace admin's credential while the run pushed with something else, which
// means the repository was cleared against one identity's permissions and worked
// on under another's — an authorisation check that proves nothing about the actor.
// Passing it in removes the possibility of that mismatch instead of documenting it.
func ResolveRepo(ctx context.Context, instruction string, allowUnlinked bool, verify RepoAccessChecker) RepoResolution {
	links, err := githubBusiness.GetAllLinkedRepos(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "codepr: list linked repos failed: %v", err)
		if res, handled := resolveExplicit(ctx, instruction, nil, allowUnlinked, verify); handled {
			return res
		}
		return RepoResolution{Found: false}
	}
	repos := make([]RepoRef, 0, len(links))
	for _, l := range links {
		if l == nil {
			continue
		}
		r := RepoRef{Owner: strings.TrimSpace(l.RepoOwner), Name: strings.TrimSpace(l.RepoName)}
		if r.Valid() {
			repos = append(repos, r)
		}
	}
	if res, handled := resolveExplicit(ctx, instruction, repos, allowUnlinked, verify); handled {
		return res
	}
	return matchRepo(instruction, repos)
}

// repoSlugRe matches a bare "owner/name" GitHub slug as a WHOLE token: a
// GitHub-legal owner (alphanumeric with internal single hyphens) and a repo name
// (alphanumeric plus - _ .). Anchored, so a path with more than one slash (a file
// path like "src/app/main.go" or a URL path) never matches here — github.com
// URLs are handled separately in parseExplicitRepoRefs.
var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

// parseExplicitRepoRefs extracts every explicitly-named "owner/name" from an
// instruction, in first-seen order, deduped case-insensitively. It recognizes
// both a github.com URL (github.com/owner/name[/...]) and a bare owner/name
// token. Pure. False positives (e.g. a "dir/file" token) are harmless: the
// caller only USES an unlinked ref after verifying the account can reach it.
func parseExplicitRepoRefs(instruction string) []RepoRef {
	var out []RepoRef
	seen := make(map[string]bool)
	add := func(owner, name string) {
		owner = strings.TrimSpace(owner)
		name = strings.TrimSpace(strings.TrimSuffix(name, ".git"))
		r := RepoRef{Owner: owner, Name: name}
		if !r.Valid() {
			return
		}
		key := strings.ToLower(r.FullName())
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	fields := strings.FieldsFunc(instruction, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', ':', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', '|', '!', '?':
			return true
		}
		return false
	})
	for _, f := range fields {
		f = strings.Trim(f, ".")
		lower := strings.ToLower(f)
		if idx := strings.Index(lower, "github.com/"); idx >= 0 {
			rest := f[idx+len("github.com/"):]
			segs := strings.Split(rest, "/")
			if len(segs) >= 2 && segs[0] != "" && segs[1] != "" {
				add(segs[0], segs[1])
			}
			continue
		}
		if repoSlugRe.MatchString(f) {
			i := strings.Index(f, "/")
			add(f[:i], f[i+1:])
		}
	}
	return out
}

// resolveExplicit handles an instruction that explicitly names one or more
// repos. It returns (res, true) only when it takes ownership of the decision:
//   - if any named repo IS in the linked set, it DEFERS (handled=false) so
//     matchRepo applies its tested linked-repo precedence;
//   - else the first named (unlinked) repo is authoritative — the user named it,
//     so we NEVER silently resolve a different linked repo:
//   - allowUnlinked off  → surfaced as UnlinkedDisabled (link it / enable it);
//   - allowUnlinked on   → access-verified and used when reachable, reported
//     as InaccessibleRepo when not, or a plain "ask" when the check errored.
//
// When no repo is named it defers to matchRepo (handled=false). verify may be
// nil (treated as "couldn't check").
func resolveExplicit(ctx context.Context, instruction string, repos []RepoRef, allowUnlinked bool, verify RepoAccessChecker) (RepoResolution, bool) {
	explicit := parseExplicitRepoRefs(instruction)
	if len(explicit) == 0 {
		return RepoResolution{}, false
	}
	linked := make(map[string]bool, len(repos))
	for _, r := range repos {
		linked[strings.ToLower(r.FullName())] = true
	}
	var firstUnlinked *RepoRef
	for i := range explicit {
		if linked[strings.ToLower(explicit[i].FullName())] {
			// A linked repo was named — let matchRepo resolve it (prefers known
			// repos and keeps its unit-tested precedence).
			return RepoResolution{}, false
		}
		if firstUnlinked == nil {
			firstUnlinked = &explicit[i]
		}
	}
	target := *firstUnlinked
	// The user explicitly named a repo that isn't linked. Honor their intent as
	// authoritative — do NOT fall through to matchRepo (which could resolve a
	// DIFFERENT linked repo via its single-repo/bare-name shortcut).
	if !allowUnlinked {
		return RepoResolution{Found: false, UnlinkedDisabled: target}, true
	}
	if verify == nil {
		return RepoResolution{Found: false, Candidates: repos}, true
	}
	ok, verr := verify(ctx, target.Owner, target.Name)
	if verr != nil {
		helpers.LogErrorWithContext(ctx, "codepr: verify repo access %s failed: %v", target.FullName(), verr)
		return RepoResolution{Found: false, Candidates: repos}, true
	}
	if ok {
		return RepoResolution{Found: true, Resolved: target}, true
	}
	return RepoResolution{Found: false, InaccessibleRepo: target, Candidates: repos}, true
}

// matchRepo deterministically resolves an instruction to one of `repos`. Pure.
//
// Resolution order (most specific first):
//  1. An explicit "owner/name" mention → resolve if exactly one matches.
//  2. A bare repo name (whole-word) → resolve if exactly one distinct repo
//     matches; if several match, they are the candidates.
//  3. No textual match: if the workspace has exactly ONE linked repo, use it
//     (unambiguous); otherwise every linked repo is a candidate ("which repo?").
func matchRepo(instruction string, repos []RepoRef) RepoResolution {
	repos = dedupeRepos(repos)
	if len(repos) == 0 {
		return RepoResolution{Found: false, NoneLinked: true}
	}
	lower := strings.ToLower(instruction)

	// 1. Explicit owner/name.
	var fullMatches []RepoRef
	for _, r := range repos {
		if containsToken(lower, strings.ToLower(r.FullName())) {
			fullMatches = append(fullMatches, r)
		}
	}
	if len(fullMatches) == 1 {
		return RepoResolution{Found: true, Resolved: fullMatches[0]}
	}
	if len(fullMatches) > 1 {
		return RepoResolution{Found: false, Candidates: fullMatches}
	}

	// 2. Bare repo name (whole-word).
	var nameMatches []RepoRef
	for _, r := range repos {
		if containsToken(lower, strings.ToLower(r.Name)) {
			nameMatches = append(nameMatches, r)
		}
	}
	if len(nameMatches) == 1 {
		return RepoResolution{Found: true, Resolved: nameMatches[0]}
	}
	if len(nameMatches) > 1 {
		return RepoResolution{Found: false, Candidates: nameMatches}
	}

	// 3. No textual match.
	if len(repos) == 1 {
		return RepoResolution{Found: true, Resolved: repos[0]}
	}
	return RepoResolution{Found: false, Candidates: repos}
}

// dedupeRepos removes duplicate repos by case-insensitive full name, preserving
// first-seen order.
func dedupeRepos(repos []RepoRef) []RepoRef {
	seen := make(map[string]bool, len(repos))
	out := make([]RepoRef, 0, len(repos))
	for _, r := range repos {
		if !r.Valid() {
			continue
		}
		key := strings.ToLower(r.FullName())
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// tokenBoundary is the set of characters that do NOT count as part of a repo
// token. Note '-', '_', '.' and '/' are treated as PART of a token, so
// "onecamp" does not match inside "onecamp-fe" and "owner/name" matches as a
// unit. Everything else (space, punctuation, quotes, backticks, line ends) is a
// boundary.
func isTokenChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' || b == '/'
}

// containsToken reports whether `token` appears in `text` delimited by non-token
// characters (or string ends), so a repo name/full-name only matches as a whole
// token, never as a substring of a longer name. Both args are expected
// lowercased by the caller. Pure.
func containsToken(text, token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	from := 0
	for {
		idx := strings.Index(text[from:], token)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(token)
		leftOK := start == 0 || !isTokenChar(text[start-1])
		rightOK := end == len(text) || !isTokenChar(text[end])
		if leftOK && rightOK {
			return true
		}
		from = start + 1
		if from >= len(text) {
			return false
		}
	}
}

// CandidateNames renders a resolution's candidate repos as "owner/name" strings
// for a needs_human question. Helper for the caller's message building.
func CandidateNames(cands []RepoRef) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.FullName())
	}
	return out
}
