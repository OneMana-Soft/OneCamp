package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// coding_git.go — git operations for the coding profile. The ARGV builders are
// pure + unit-tested (the security-critical invariants live here); the thin exec
// wrapper runs them in the checkout with a bounded deadline and a secret-free
// environment.
//
// Invariants enforced in the builders (not left to the caller):
//   - clone is shallow (depth 1) + blobless (partial) + optionally sparse, so
//     repo SIZE never dictates checkout cost.
//   - a fresh head branch is created; NewBranchArgs REFUSES head == base.
//   - push targets the head branch ONLY and is NEVER --force, so a run can never
//     overwrite the base branch or someone else's work.
//   - the clone URL carries the token for auth but is REDACTED in any error.

// repoCloneURL builds the CLEAN https clone URL (no credentials) for a repo. The
// token is NEVER embedded in the URL — doing so would persist it into the
// checkout's .git/config, where any build step or test could read it. Auth is
// supplied per-command via authHeaderConfigArgs instead. Returns an error for a
// missing owner/name so we never clone from an unexpected target.
func repoCloneURL(host, owner, name string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "github.com"
	}
	if owner == "" || name == "" {
		return "", fmt.Errorf("repo owner and name are required")
	}
	u := &url.URL{
		Scheme: "https",
		Host:   host,
		Path:   fmt.Sprintf("/%s/%s.git", owner, name),
	}
	return u.String(), nil
}

// authHeaderConfigArgs returns the per-command git config that supplies the
// clone/push credential as an HTTP Authorization header — WITHOUT persisting it
// to .git/config (a `-c` config is scoped to that single invocation). The token
// is sent as HTTP basic `x-access-token:<token>` (works for GitHub PAT / OAuth /
// installation tokens). Empty token ⇒ nil (an unauthenticated clone, which fails
// fast on a private repo rather than hanging). These args MUST precede the git
// subcommand (git -c <cfg> clone …).
func authHeaderConfigArgs(token string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"-c", "http.extraheader=AUTHORIZATION: basic " + basic}
}

// cloneArgs builds a shallow + partial (blobless) + optionally sparse clone.
// depth<=0 defaults to 1. When sparse is requested the clone uses
// --no-checkout + --sparse so only the sparse set is materialized later.
func cloneArgs(cloneURL, baseBranch, dest string, depth int, sparse bool) []string {
	if depth <= 0 {
		depth = 1
	}
	args := []string{
		"clone",
		"--depth", fmt.Sprintf("%d", depth),
		"--filter=blob:none",
		"--single-branch",
	}
	if b := strings.TrimSpace(baseBranch); b != "" {
		args = append(args, "--branch", b)
	}
	if sparse {
		args = append(args, "--no-checkout", "--sparse")
	}
	args = append(args, cloneURL, dest)
	return args
}

// sparseSetArgs restricts the working tree to the given paths (cone mode off, so
// exact files/dirs). Empty paths ⇒ nil (caller skips sparse).
func sparseSetArgs(paths []string) []string {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" && !strings.HasPrefix(p, "-") {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return append([]string{"sparse-checkout", "set", "--no-cone"}, clean...)
}

// newBranchArgs creates+checks-out a fresh head branch. It REFUSES head == base
// (case-insensitive) so a run can never commit onto the base branch — the single
// most important push-safety invariant.
func newBranchArgs(head, base string) ([]string, error) {
	head = strings.TrimSpace(head)
	base = strings.TrimSpace(base)
	if head == "" {
		return nil, fmt.Errorf("a head branch name is required")
	}
	if strings.EqualFold(head, base) {
		return nil, fmt.Errorf("refusing to work on the base branch %q", base)
	}
	if strings.HasPrefix(head, "-") {
		return nil, fmt.Errorf("invalid head branch name")
	}
	return []string{"checkout", "-b", head}, nil
}

// checkoutBranch is the branch a run clones: the existing head branch when it
// continues one, otherwise the base it starts from. Pure.
func checkoutBranch(job CodingJob) string {
	if job.ContinueBranch {
		return job.HeadBranch
	}
	return job.BaseBranch
}

// startBranchArgs is the git command that puts a run on its head branch:
// creating it from the base, or nothing when it continues an existing head
// branch (the clone is already on it). The head branch is validated the same
// way either way: never the base, never an option. Pure.
func startBranchArgs(job CodingJob) ([]string, error) {
	nb, err := newBranchArgs(job.HeadBranch, job.BaseBranch)
	if err != nil {
		return nil, err
	}
	if job.ContinueBranch {
		return nil, nil
	}
	return nb, nil
}

// commitArgs stages everything and commits with a standardized identity/message.
// Returns the two commands (add, commit) since commit config is passed inline
// via -c so we never mutate global git config.
func commitArgs(message string) ([][]string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Automated change by OneCamp AI"
	}
	add := []string{"add", "-A"}
	commit := []string{
		"-c", "user.name=OneCamp AI",
		"-c", "user.email=ai@onecamp.local",
		"commit", "--no-verify", "-m", message,
	}
	return [][]string{add, commit}, nil
}

// pushArgs pushes ONLY the head branch to origin, never --force. head is
// validated (non-empty, not a flag).
func pushArgs(head string) ([]string, error) {
	head = strings.TrimSpace(head)
	if head == "" || strings.HasPrefix(head, "-") {
		return nil, fmt.Errorf("invalid head branch for push")
	}
	// refs/heads/<head>:refs/heads/<head> is explicit and unambiguous; no force.
	ref := fmt.Sprintf("refs/heads/%s:refs/heads/%s", head, head)
	return []string{"push", "--set-upstream", "origin", ref}, nil
}

// stageArgs stages every change in the work tree, INCLUDING files the run
// created. It must run before the change is measured: `git diff` compares the
// work tree to HEAD and an untracked file appears in NEITHER, so a run whose
// whole change was new files measured as zero and was thrown away as "I couldn't
// produce a change" — with the branch never pushed and the checkout deleted.
// Respects .gitignore, so ignored build output is still excluded.
func stageArgs() []string { return []string{"add", "-A"} }

// diffArgs / diffStatArgs read the change the run produced against the base
// commit. Both read the INDEX (--cached), not the work tree, so newly created
// files are counted and shown; stageArgs must have run first. Reading the work
// tree instead was the bug described on stageArgs.
func diffArgs() []string { return []string{"diff", "--no-color", "--cached"} }

func diffStatArgs() []string { return []string{"diff", "--numstat", "--cached"} }

// gitEnv builds the environment for a git child: minimal, secret-free, and
// non-interactive (credential helpers + prompts disabled so a missing token fails
// fast instead of hanging), PLUS the egress route.
//
// The route is not optional. In the shipped topology the runner has no direct
// internet and reaches the git host only through the allowlisting proxy named in
// HTTP(S)_PROXY, so a git child without those variables cannot clone or push at
// all. Extracted as a pure function so that invariant is unit-tested rather than
// discovered in production.
func gitEnv(dir string) []string {
	return append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + dir,
		"GIT_TERMINAL_PROMPT=0", // never prompt for credentials (fail fast)
		"GIT_CONFIG_NOSYSTEM=1", // ignore /etc/gitconfig
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GCM_INTERACTIVE=never",
	}, proxyEnv()...)
}

// runGit executes a git command in dir with a bounded deadline and the
// environment gitEnv builds. Returns combined output (capped by the caller's
// reader) and any error.
func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv(dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return string(out), nil
}
