package codesandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// run.go — the orchestration core: gate a run against the master switch + budget
// tiers, assemble a self-contained Job (preamble + code + injected files +
// clamped limits), submit it to the Runner, and classify the Result into a
// caller-ready Outcome (chart blocks / file artifacts / stdout / a sanitized
// failure message + usage to meter and audit).
//
// It is written with INJECTED dependencies — the caller does the DB/permission
// work (resolve inputs, fetch settings + today's usage) and passes the results
// in — so this logic is exhaustively unit-testable with MockRunner and never
// itself touches the database. The thin, impure caller (the run_analysis tool)
// wires resolution, persistence, and attachment creation around it.

// RunParams is everything Run needs, with all DB/permission work already done.
type RunParams struct {
	JobID    string
	Language Language
	Code     string
	// Inputs are already resolved, permission-checked, and volume-bounded by the
	// caller; Run only serializes/injects them.
	Inputs []InputFile
	// Enabled is the sandbox master switch (also the kill switch).
	Enabled bool
	// EstSeconds is the caller's estimate of the run cost for budgeting
	// (<=0 = unknown; only the run-count dimension is then checked).
	EstSeconds int
	// Tiers are the pre-fetched caps+usage for the applicable budget tiers
	// (agent/channel/workspace/user), evaluated in order.
	Tiers []Tier
	// Limits are the per-run resource limits (clamped to safety ceilings here).
	Limits Limits
	// Runner executes the job. A nil Runner means the sandbox is configured on
	// but no runner is reachable → the run is reported unavailable.
	Runner Runner
}

// Outcome is the caller-ready result of a Run. Exactly one of {Refused, an OK
// result, a failed result} holds. Charts are ready chart-spec JSON strings
// (emit each as a ```chart block); Files are artifacts to persist as
// attachments; Message is a user-facing note for a refusal/failure ("" on a
// clean success).
type Outcome struct {
	Refused    bool
	Reason     StopReason
	Status     RunStatus
	Charts     []string
	Files      []Artifact
	Stdout     string
	Message    string
	Usage      Usage
	CodeSHA256 string
}

// refused builds a refusal outcome with a reason + message.
func refused(reason StopReason, msg string) Outcome {
	return Outcome{Refused: true, Reason: reason, Message: msg}
}

// Run orchestrates one sandbox run. It never panics and never blocks: a nil or
// failing Runner degrades to a refusal/unavailable outcome.
func Run(ctx context.Context, p RunParams) Outcome {
	codeSHA := hashCode(p.Code)

	// 1. Language must be supported.
	if !ValidLanguage(p.Language) {
		return Outcome{Refused: true, Reason: StopReasonUnavailable, CodeSHA256: codeSHA,
			Message: "This analysis language isn't supported."}
	}

	// 2. Budget gate (includes the master enable switch).
	if d := CheckBudget(p.Enabled, p.EstSeconds, p.Tiers); !d.Allowed {
		out := refused(d.Reason, budgetMessage(d.Reason))
		out.CodeSHA256 = codeSHA
		return out
	}

	// 3. Runner must be reachable.
	if p.Runner == nil {
		out := refused(StopReasonUnavailable, "The analysis sandbox isn't available right now.")
		out.CodeSHA256 = codeSHA
		return out
	}

	// 4. Assemble a self-contained job: preamble that loads the injected inputs,
	//    then the user's code; files mounted read-only under /data; clamped
	//    limits.
	prepared := PrepareInputFiles(p.Inputs)
	preamble := BuildPythonPreamble(prepared)
	code := p.Code
	if preamble != "" {
		code = preamble + "\n" + code
	}
	job := Job{
		ID:       p.JobID,
		Language: p.Language,
		Code:     code,
		Files:    FilesMap(prepared),
		Limits:   ClampLimits(p.Limits),
	}

	// 5. Execute.
	res, err := p.Runner.Run(ctx, job)
	if err != nil {
		out := refused(StopReasonUnavailable, "The analysis sandbox couldn't run this right now.")
		out.CodeSHA256 = codeSHA
		return out
	}

	// 6. Classify the result.
	out := Outcome{Status: res.Status, Usage: res.Usage, CodeSHA256: codeSHA}
	if !res.Status.Succeeded() {
		out.Message = FailureMessage(res)
		return out
	}
	out.Stdout = strings.TrimSpace(res.Stdout)
	for _, a := range res.Artifacts {
		if spec, ok := ChartSpecJSON(a); ok {
			out.Charts = append(out.Charts, spec)
			continue
		}
		// Anything that isn't a valid chart is surfaced as a file artifact.
		out.Files = append(out.Files, a)
	}
	return out
}

// hashCode returns the hex SHA-256 of the user's code, for audit (the full code
// lives in the agent transcript; the ledger stores only the hash).
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// budgetMessage maps a refusal reason to a concise user-facing note.
func budgetMessage(r StopReason) string {
	switch r {
	case StopReasonDisabled:
		return "The analysis sandbox is turned off for this workspace."
	case StopReasonAgentBudget, StopReasonChannelBudget, StopReasonWorkspaceBudget, StopReasonUserBudget:
		return "The analysis budget for today has been reached. Try again tomorrow or ask an admin to raise the limit."
	case StopReasonBusy:
		return "The analysis sandbox is busy right now. Try again in a moment."
	default:
		return "The analysis couldn't be run right now."
	}
}
