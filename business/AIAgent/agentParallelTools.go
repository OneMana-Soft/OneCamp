package business

// Running a turn's LOOKUPS at the same time.
//
// A model routinely asks for several independent reads in one turn ("list the
// repos, search the channel, get the task"), and the loop executed them strictly
// one after another. Each MCP/API round trip is hundreds of milliseconds to
// seconds, so a three-lookup step took the sum of all three while the person
// watched a "working…" comment — for calls that don't depend on each other at all.
//
// Only READ-ONLY calls are ever run concurrently, which is what makes this safe
// rather than merely fast:
//   - order is irrelevant for reads: nothing observes another call's effect, so
//     concurrency cannot change what the model sees (results are still recorded
//     and fed back in the model's original call order);
//   - no governance decision is bypassed: a write, an approval-gated call, a
//     destructive call, a duplicate, an out-of-scope target or a control tool is
//     never eligible, so it still runs through the sequential path with every
//     check in place;
//   - the executors re-check the acting user's permissions per call, exactly as
//     before, and each MCP call already builds its own client.
//
// Implementation shape: a pre-pass fetches eligible reads and caches them by
// action signature; the sequential loop is unchanged and simply consumes a cached
// result instead of calling the executor. That keeps ONE copy of the policy logic
// (the sequential path) — a second, subtly different copy is how a parallel fast
// path quietly becomes a permission bypass.

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// defaultAgentParallelReads bounds how many lookups one turn may have in flight.
// Small on purpose: the win is going from serial to a handful at once, while a
// larger fan-out mostly risks rate-limiting the very APIs being read.
const defaultAgentParallelReads = 4

// agentParallelReads is the per-turn read concurrency. AI_AGENT_PARALLEL_READS
// tunes it; 1 disables the fast path entirely (an operations kill switch that
// needs no redeploy of behaviour, just a value).
func agentParallelReads() int {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_PARALLEL_READS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return defaultAgentParallelReads
}

// prefetchedResult is one pre-executed tool call, kept exactly as the executor
// returned it so the sequential path records it identically to a direct call.
type prefetchedResult struct {
	result string
	meta   map[string]string
	err    error
}

// prefetchReadOnlyTools runs the eligible read-only calls of one turn
// concurrently and returns them keyed by action signature.
//
// eligible is supplied by the caller (it owns the allow-list, scope, dedupe and
// dry-run state) and MUST only accept calls the sequential path would execute
// verbatim. Returns nil when there is nothing to gain — one call, or none — so a
// single-lookup turn behaves exactly as it did before.
func prefetchReadOnlyTools(ctx context.Context, actions []ai.ProposedAction, userUUID string, eligible func(ai.ProposedAction) bool) map[string]prefetchedResult {
	limit := agentParallelReads()
	if limit < 2 || len(actions) < 2 {
		return nil
	}
	var pending []ai.ProposedAction
	claimed := map[string]bool{}
	for _, a := range actions {
		sig := ai.ActionSignature(a)
		// Only the FIRST occurrence of a signature is fetched: a repeat in the
		// same turn is a duplicate the sequential path skips anyway.
		if claimed[sig] || !eligible(a) {
			continue
		}
		claimed[sig] = true
		pending = append(pending, a)
	}
	if len(pending) < 2 {
		return nil
	}

	out := make(map[string]prefetchedResult, len(pending))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, a := range pending {
		wg.Add(1)
		go func(a ai.ProposedAction) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, meta, err := runToolSafely(ctx, a, userUUID)
			mu.Lock()
			out[ai.ActionSignature(a)] = prefetchedResult{result: res, meta: meta, err: err}
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	helpers.LogInfoWithContext(ctx, "agentRunner: ran %d lookups concurrently", len(pending))
	return out
}

// runToolSafely executes one tool call, turning a panicking executor into an
// ordinary tool error. This is not defensive noise: the runner's own recover
// cannot catch a panic raised on another goroutine, so without this a single
// misbehaving connector would take the whole server down instead of failing one
// lookup.
func runToolSafely(ctx context.Context, a ai.ProposedAction, userUUID string) (res string, meta map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("agentRunner: recovered panic in tool %s: %v", a.ToolName, r)
			res, meta, err = "", nil, errors.New("this tool hit an internal error")
		}
	}()
	exec, ok := ai.GetExecutor(a.ToolName)
	if !ok {
		return "", nil, errors.New("this tool is currently unavailable")
	}
	return exec(ctx, a, userUUID)
}

// isControlTool reports whether a call is one of the registry-free control tools
// the loop handles itself (pausing for a human, saving its own progress,
// remembering a standing instruction, managing its routines). They have no
// executor and change the agent's OWN state, so they are never pre-run — and
// keeping the list in one predicate means a new control tool can't be missed by
// the concurrency gate.
func isControlTool(name string) bool {
	switch name {
	case blockerToolName, progressToolName, rememberToolName, forgetToolName:
		return true
	}
	return isRoutineTool(name)
}
