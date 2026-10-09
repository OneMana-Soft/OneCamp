package business

// sandboxExecutor.go — the run_analysis tool: the thin, impure caller that wires
// permission-checked input resolution, budget gating, execution, persistence,
// and result formatting around the pure codesandbox.Run orchestrator.
//
// It runs AS the acting user (agent owner): every table read is permission-
// checked exactly like read_table/query_table. It performs NO external side
// effects — the sandbox is network-less and its filesystem is ephemeral — so
// the tool is side-effect-free (read-only), but it is still gated by the admin
// sandbox switch and metered against the sandbox budget.
//
// Until the code-runner sidecar (spec Task 6) is wired, sandboxRunnerFor returns
// nil and every call degrades to a clean "sandbox unavailable" result — the
// whole path (gating, resolution, budgets, audit, formatting) is exercised and
// correct; only the actual execution is pending.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	codesandbox "github.com/akashc777/OneCamp/business/CodeSandbox"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// sandboxInputMaxRows bounds how many rows a raw-table input binding pulls into
// the sandbox, mirroring the aggregate scan cap so a run can't ingest an
// unbounded dataset.
const sandboxInputMaxRows = 5000

// maxSandboxInputs caps the number of input bindings one run may request.
const maxSandboxInputs = 8

// sandboxBindingParam is one requested input in the tool call.
type sandboxBindingParam struct {
	Name      string                       `json:"name"`
	TableUUID string                       `json:"table_uuid"`
	Query     *dataTableBusiness.QuerySpec `json:"query,omitempty"`
}

// executeRunAnalysis is the run_analysis tool executor.
func executeRunAnalysis(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	code := strings.TrimSpace(action.Params["code"])
	if code == "" {
		return "", nil, fmt.Errorf("code is required")
	}

	settings, serr := aiModels.GetSettings(ctx)
	if serr != nil {
		return "", nil, fmt.Errorf("could not read AI settings")
	}

	ctx, actor, aerr := tableActor(ctx, userUUID)
	if aerr != nil {
		return "", nil, aerr
	}

	// Resolve the requested input bindings to permission-checked, bounded CSV
	// files. An unauthorized/oversized/invalid binding fails the whole run.
	inputs, ierr := resolveSandboxInputs(ctx, action.Params["inputs"], actor)
	if ierr != nil {
		return ierr.Error(), nil, nil // actionable feedback the model can correct
	}

	limits := codesandbox.DefaultLimits()
	tiers := buildSandboxTiers(ctx, settings)

	out := codesandbox.Run(ctx, codesandbox.RunParams{
		JobID:      uuid.NewString(),
		Language:   codesandbox.LanguagePython,
		Code:       code,
		Inputs:     inputs,
		Enabled:    settings.SandboxEnabled,
		EstSeconds: int(limits.Wall.Seconds()),
		Tiers:      tiers,
		Limits:     limits,
		Runner:     sandboxRunnerFor(settings),
	})

	// Audit every run (any outcome), best-effort — never block the reply.
	recordSandboxRun(ctx, actor, out, ai.SandboxScopeFromContext(ctx))

	return formatSandboxOutcome(out), nil, nil
}

// resolveSandboxInputs parses the inputs param and resolves each binding to a
// CSV InputFile, enforcing the actor's permissions on every table it reads.
func resolveSandboxInputs(ctx context.Context, raw string, actor dataTableBusiness.Actor) ([]codesandbox.InputFile, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var bindings []sandboxBindingParam
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return nil, fmt.Errorf("inputs must be a JSON array of {\"name\":..,\"table_uuid\":..,\"query\":{..}?}")
	}
	if len(bindings) > maxSandboxInputs {
		return nil, fmt.Errorf("too many inputs (max %d)", maxSandboxInputs)
	}

	out := make([]codesandbox.InputFile, 0, len(bindings))
	for i, b := range bindings {
		name := strings.TrimSpace(b.Name)
		if name == "" {
			name = fmt.Sprintf("input_%d", i+1)
		}
		tableID, perr := uuid.Parse(strings.TrimSpace(b.TableUUID))
		if perr != nil {
			return nil, fmt.Errorf("input %q needs a valid table_uuid (use list_tables to find it)", name)
		}

		if b.Query != nil {
			res, _, qerr := dataTableBusiness.AggregateTable(ctx, tableID, actor, *b.Query)
			if qerr != nil {
				return nil, sandboxResolveErr(name, qerr)
			}
			out = append(out, aggResultToInputFile(name, res))
			continue
		}

		bundle, berr := dataTableBusiness.GetBundle(ctx, tableID, actor)
		if berr != nil {
			return nil, sandboxResolveErr(name, berr)
		}
		rows, rerr := dataTableBusiness.ListRows(ctx, tableID, actor, sandboxInputMaxRows, 0)
		if rerr != nil {
			return nil, sandboxResolveErr(name, rerr)
		}
		out = append(out, tableRowsToInputFile(name, bundle.Fields, rows, sandboxInputMaxRows))
	}
	return out, nil
}

// sandboxResolveErr maps a DataTable error to actionable model-facing feedback.
func sandboxResolveErr(name string, err error) error {
	if dataTableBusiness.IsForbidden(err) {
		return fmt.Errorf("input %q: you don't have access to that table", name)
	}
	if dataTableBusiness.IsNotFound(err) {
		return fmt.Errorf("input %q: table not found", name)
	}
	return fmt.Errorf("input %q: %s", name, err.Error())
}

// buildSandboxTiers assembles the enforceable budget tiers from settings +
// today's usage, ordered most-specific-first (agent → channel → workspace) so
// CheckBudget reports the tightest tier that blocks. The per-agent tier comes
// from the agent row (threaded via the sandbox run scope); the per-channel tier
// from the workspace-wide channel caps applied to this run's channel; the
// workspace tier is the global safety cap. A tier is added only when it has a
// cap AND its usage query succeeds (fail-open: a usage-read error drops just
// that tier rather than blocking the run — the remaining tiers still bound it).
func buildSandboxTiers(ctx context.Context, s *aiModels.AISettings) []codesandbox.Tier {
	scope := ai.SandboxScopeFromContext(ctx)
	var tiers []codesandbox.Tier

	// Per-agent tier (most specific).
	if scope.AgentID != "" && (scope.AgentDailySeconds > 0 || scope.AgentDailyRuns > 0) {
		if agentID, perr := uuid.Parse(scope.AgentID); perr == nil {
			if u, err := aiModels.AgentSandboxUsageToday(ctx, agentID); err == nil {
				tiers = append(tiers, codesandbox.Tier{
					Reason: codesandbox.StopReasonAgentBudget,
					Caps:   codesandbox.Caps{DailySeconds: scope.AgentDailySeconds, DailyRuns: scope.AgentDailyRuns},
					Usage:  codesandbox.TierUsage{Seconds: u.Seconds, Runs: u.Runs},
				})
			}
		}
	}

	// Per-channel tier (workspace-wide channel caps applied to this channel).
	if scope.ChannelID != "" && (s.SandboxChannelDailySeconds > 0 || s.SandboxChannelDailyRuns > 0) {
		if channelID, perr := uuid.Parse(scope.ChannelID); perr == nil {
			if u, err := aiModels.ChannelSandboxUsageToday(ctx, channelID); err == nil {
				tiers = append(tiers, codesandbox.Tier{
					Reason: codesandbox.StopReasonChannelBudget,
					Caps:   codesandbox.Caps{DailySeconds: s.SandboxChannelDailySeconds, DailyRuns: s.SandboxChannelDailyRuns},
					Usage:  codesandbox.TierUsage{Seconds: u.Seconds, Runs: u.Runs},
				})
			}
		}
	}

	// Workspace tier (global cap).
	if s.SandboxWorkspaceDailySeconds > 0 || s.SandboxWorkspaceDailyRuns > 0 {
		if u, err := aiModels.WorkspaceSandboxUsageToday(ctx); err == nil {
			tiers = append(tiers, codesandbox.Tier{
				Reason: codesandbox.StopReasonWorkspaceBudget,
				Caps:   codesandbox.Caps{DailySeconds: s.SandboxWorkspaceDailySeconds, DailyRuns: s.SandboxWorkspaceDailyRuns},
				Usage:  codesandbox.TierUsage{Seconds: u.Seconds, Runs: u.Runs},
			})
		}
	}
	return tiers
}

// recordSandboxRun writes the audit/usage row for a completed or refused run,
// attributing it to the agent/channel/run from the sandbox scope (each optional
// — a NULL id for the assistant path). Best-effort: a write failure is logged,
// never surfaced.
func recordSandboxRun(ctx context.Context, actor dataTableBusiness.Actor, out codesandbox.Outcome, scope ai.SandboxScope) {
	status := string(out.Status)
	if out.Refused {
		status = string(out.Reason)
	}
	row := &aiModels.SandboxRun{
		ActorID:       actor.UserID,
		CodeSHA256:    out.CodeSHA256,
		Status:        status,
		WallMS:        out.Usage.WallMS,
		CPUMS:         out.Usage.CPUMS,
		PeakMemBytes:  out.Usage.PeakMemBytes,
		ArtifactCount: len(out.Charts) + len(out.Files),
	}
	if id, err := uuid.Parse(scope.AgentID); err == nil {
		row.AgentID = &id
	}
	if id, err := uuid.Parse(scope.ChannelID); err == nil {
		row.ChannelID = &id
	}
	if id, err := uuid.Parse(scope.RunID); err == nil {
		row.RunID = &id
	}
	if _, err := aiModels.RecordSandboxRun(ctx, row); err != nil {
		helpers.LogErrorWithContext(ctx, "run_analysis: record sandbox run failed: %v", err)
	}
}

// formatSandboxOutcome renders the outcome for the model: a refusal/failure
// message, or the stdout summary + any ready chart blocks (drawn inline by the
// frontend). File artifacts are noted by name (attachment persistence lands
// with the sidecar in Task 6).
func formatSandboxOutcome(out codesandbox.Outcome) string {
	if out.Refused || !out.Status.Succeeded() {
		if out.Message != "" {
			return out.Message
		}
		return "The analysis could not be completed."
	}
	var b strings.Builder
	if out.Stdout != "" {
		b.WriteString(out.Stdout)
		b.WriteString("\n")
	}
	for _, chart := range out.Charts {
		b.WriteString("\n```chart\n")
		b.WriteString(chart)
		b.WriteString("\n```\n")
	}
	for _, f := range out.Files {
		b.WriteString(fmt.Sprintf("\n(Produced file: %s)\n", f.Name))
	}
	res := strings.TrimSpace(b.String())
	if res == "" {
		return "The analysis ran but produced no output."
	}
	return res
}

// TestSandbox runs a trivial probe against the currently-configured code-runner
// sidecar to validate the deployment (reachability, auth token, execution under
// limits) WITHOUT requiring the sandbox master switch to be enabled — so an
// admin can verify the runner before flipping it on. Reads the saved settings,
// so save the runner URL/token first, then test. Never executes anything the
// admin didn't ask for: a fixed, input-less probe.
func TestSandbox(ctx context.Context) adapter.SandboxTestResult {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return adapter.SandboxTestResult{Ok: false, Status: "error", Message: "could not read AI settings"}
	}
	runner := sandboxRunnerFor(settings)
	if runner == nil {
		return adapter.SandboxTestResult{
			Ok:      false,
			Status:  "unconfigured",
			Message: "Set and save a runner URL before testing.",
		}
	}

	limits := codesandbox.ClampLimits(codesandbox.Limits{})
	job := codesandbox.Job{
		ID:       uuid.NewString(),
		Language: codesandbox.LanguagePython,
		Code:     "print('OneCamp sandbox self-test OK')",
		Limits:   limits,
	}

	res, rerr := runner.Run(ctx, job)
	if rerr != nil {
		return adapter.SandboxTestResult{
			Ok:      false,
			Status:  "unreachable",
			Message: fmt.Sprintf("Could not reach the code-runner: %s", rerr.Error()),
		}
	}
	if !res.Status.Succeeded() {
		return adapter.SandboxTestResult{
			Ok:      false,
			Status:  string(res.Status),
			Message: fmt.Sprintf("The runner responded but the probe did not complete (%s).", res.Status),
			WallMS:  res.Usage.WallMS,
		}
	}
	return adapter.SandboxTestResult{
		Ok:      true,
		Status:  string(res.Status),
		Message: "The code-runner executed the probe successfully.",
		WallMS:  res.Usage.WallMS,
	}
}

// sandboxRunnerFor returns the code-runner client for the configured endpoint,
// or nil when the sandbox has no runner wired (empty URL) — in which case
// codesandbox.Run degrades every call to a clean "unavailable" result. The
// shared token is decrypted here (stored encrypted, like other AI secrets) and
// never logged.
func sandboxRunnerFor(s *aiModels.AISettings) codesandbox.Runner {
	if s == nil || strings.TrimSpace(s.SandboxRunnerURL) == "" {
		return nil
	}
	token := ""
	if len(s.SandboxRunnerTokenEnc) > 0 {
		if dec, err := aiModels.DecryptAPIKey(s.SandboxRunnerTokenEnc); err == nil {
			token = dec
		}
	}
	return codesandbox.NewHTTPRunner(s.SandboxRunnerURL, token)
}
