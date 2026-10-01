package codepr

import (
	"fmt"
	"strings"
)

// Stages of a coding run, reported as it goes.
//
// A run takes minutes (clone, edit, build, test), and all a person used to see
// between "On it" and the pull request was silence, which people cannot tell
// apart from a hang; the mature coding agents show where they are (a live
// checklist, a session log) for exactly that reason. The orchestrator names
// each stage as it starts; the caller decides where to show it.

// Stage is one step of a run that a person would recognise.
type Stage string

const (
	// StageWorking: the repository is known and allowed, and the isolated
	// runner is making and verifying the change. The long part.
	StageWorking Stage = "working"
	// StageChecking: the change exists; checking it stays within the task.
	StageChecking Stage = "checking"
	// StageOpening: opening the pull request.
	StageOpening Stage = "opening"
)

// StageUpdate is what the orchestrator knows when a stage starts. Result is
// set from StageChecking on, once the runner has produced a change.
type StageUpdate struct {
	Stage  Stage
	Repo   RepoRef
	Result *CodingResult
}

// StageMessage is how a stage reads to a person, with the evidence the run
// has so far. Pure.
func StageMessage(u StageUpdate) string {
	repo := strings.TrimSpace(u.Repo.Owner + "/" + u.Repo.Name)
	if repo == "/" {
		repo = "the repository"
	}
	switch u.Stage {
	case StageWorking:
		return fmt.Sprintf("Working in %s: making the change in an isolated copy, then running its build and tests. This usually takes a few minutes.", repo)
	case StageChecking:
		return "The change is made. " + evidence(u.Result) + " Checking it stays within what was asked…"
	case StageOpening:
		return "Opening the pull request in " + repo + "…"
	}
	return ""
}

// evidence says what the runner produced and what it verified. Pure.
func evidence(r *CodingResult) string {
	if r == nil {
		return ""
	}
	parts := []string{}
	if d := r.DiffStat; d.Files > 0 {
		parts = append(parts, fmt.Sprintf("%d file%s changed (+%d −%d)", d.Files, plural(d.Files), d.Added, d.Removed))
	}
	v := r.Verifier
	switch {
	case len(v.Ran) > 0 && v.AllPassed && v.HadTests:
		parts = append(parts, "the build and tests pass")
	case len(v.Ran) > 0 && v.AllPassed:
		parts = append(parts, "the build passes (the repository has no tests)")
	case len(v.Ran) > 0:
		parts = append(parts, "some checks did not pass")
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, "; ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// stage reports a stage when a listener is set. Nil-safe.
func (o *Orchestrator) stage(s Stage, repo RepoRef, result *CodingResult) {
	if o.OnStage != nil {
		o.OnStage(StageUpdate{Stage: s, Repo: repo, Result: result})
	}
}
