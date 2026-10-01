package business

// Recording what an agent is about to do, before it does it.
//
// WHY THIS EXISTS. The evidence pack stated its own limit honestly: a hash chain
// proves the log was not altered, and proves nothing about whether the log
// recorded everything. That limit was real. A run's tool calls lived in memory
// and reached the database only when the run finished, so a process that died
// mid-run left actions that had genuinely happened, a message sent or an issue
// commented on, with no record anywhere.
//
// Writing intent first inverts which way the uncertainty falls. After a crash the
// log may name an action that never took effect; it can no longer miss one that
// did. An auditor told "this was intended and the outcome is unknown" is being
// told the truth. Silence was not.
//
// FAIL CLOSED. If the intent cannot be written, the action does not run. That is
// an availability cost paid deliberately: the claim this table exists to support
// is "an effecting action cannot have happened without a record", and a claim
// that a database blip can quietly break is not a claim. The agent surfaces the
// refusal as a tool error and carries on, which is a path it already handles.
//
// EFFECTING CALLS ONLY. A read that went unrecorded cannot mean an unrecorded
// change, and a synchronous write per lookup would buy nothing. The claim is
// about effects, and is written that way wherever it is stated.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	actionLog "github.com/akashc777/OneCamp/models/postgres/AgentActionLog"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// ErrIntentNotRecorded is returned when the intent could not be persisted, so
// the caller must not perform the action.
var ErrIntentNotRecorded = errors.New("could not record this action before running it, so it was not run")

// paramsDigest fingerprints a call's parameters without keeping them.
//
// Sorted keys so the same call digests the same regardless of map iteration
// order, which is random in Go and would otherwise make two identical calls look
// like two different ones.
func paramsDigest(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(params[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recordActionIntent persists what is about to happen and returns the row to
// close afterwards. A nil id means nothing needs closing.
//
// Returns ErrIntentNotRecorded when the write fails, and the caller MUST NOT act.
func recordActionIntent(ctx context.Context, runID *uuid.UUID, agentID uuid.UUID, runAsUserID *uuid.UUID, toolName string, params map[string]string) (*uuid.UUID, error) {
	if ai.ToolIsReadOnly(toolName) {
		return nil, nil
	}

	id, err := recordIntentSafely(ctx, runID, agentID, runAsUserID, toolName, paramsDigest(params))
	if err != nil {
		return nil, ErrIntentNotRecorded
	}
	return &id, nil
}

// recordIntentSafely turns a panic into the same refusal a failed write produces.
//
// This runs on the agent's hot path, ahead of every effecting call, and it
// dereferences a database handle that is nil before initialisation completes. A
// panic here would take down the process; more importantly, ANY failure to
// record must read as "do not act", and a panic is a failure to record.
func recordIntentSafely(ctx context.Context, runID *uuid.UUID, agentID uuid.UUID, runAsUserID *uuid.UUID, toolName, digest string) (id uuid.UUID, err error) {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx,
				"recordActionIntent recovered panic for tool %s: %v\n%s", toolName, r, debug.Stack())
			id, err = uuid.Nil, ErrIntentNotRecorded
		}
	}()
	in := actionLog.Intent{
		ID:           uuid.New(),
		RunID:        runID,
		AgentID:      agentID,
		RunAsUserID:  runAsUserID,
		ToolName:     toolName,
		ParamsDigest: digest,
	}
	signIntent(&in, time.Now())
	if err := actionLog.RecordIntent(ctx, in); err != nil {
		return uuid.Nil, err
	}
	return in.ID, nil
}

// closeActionIntent records how the attempt ended.
//
// Deliberately swallows its own error. A lost outcome leaves the row unresolved,
// which reads as "we do not know" and is the honest answer; failing the tool call
// because the bookkeeping write failed would turn a completed action into a
// reported failure, which is a worse lie than the one this prevents.
func closeActionIntent(ctx context.Context, id *uuid.UUID, outcome string, errText string) {
	if id == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx,
				"closeActionIntent recovered panic: %v\n%s", r, debug.Stack())
		}
	}()

	var msg *string
	if strings.TrimSpace(errText) != "" {
		msg = &errText
	}
	_ = actionLog.RecordOutcome(ctx, *id, outcome, msg)
}
