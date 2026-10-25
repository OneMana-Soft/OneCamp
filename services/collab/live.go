// Package collab is the API's client for the collaboration service.
package collab

// The Live Doc Writer: every content change to an existing doc goes through the
// live Yjs copy the collaboration service holds (docs programme, PROGRAM.md §4 #9
// and invariants I1–I3). Each op is one Yjs transaction on the server's copy, so
// people with the doc open see it arrive and nothing they type is overwritten. It
// is attributed from server context (I11), can mark agent edits and keep a version
// of the doc first, and is stored at once. The service answers these on an
// internal port only (collaboration-service live/server.js, LIVE_PORT), never
// through the public host.
//
// The caller decides first whether the person (or the agent, for the person who
// asked it) may make the change: the service trusts the API.
//
// NO RETRIES. A write is not idempotent (an append sent twice adds its blocks
// twice), so a call that fails is reported, never repeated here; a 4xx is an
// answer, not a fault. A caller whose write is safe to repeat (a seed, which
// refuses a doc that has content) may try again itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

var (
	// ErrUnavailable is a write the service could not make now: it is not
	// reachable, not configured, or could not read the doc. Try again later;
	// never write the body underneath instead.
	ErrUnavailable = errors.New("live document writes are not available just now")
	// ErrNoSuchDoc is a doc the service's API lookup does not know.
	ErrNoSuchDoc = errors.New("live write: no such doc")
	// ErrNoSuchBlock is a block id the doc does not hold.
	ErrNoSuchBlock = errors.New("live write: no such block")
	// ErrHasContent is a seed refused because the doc already holds something.
	ErrHasContent = errors.New("live write: the doc already has content")
	// ErrDocInTrash is a doc in the trash: it takes no writes.
	ErrDocInTrash = errors.New("live write: the doc is in the trash")
	// ErrRefused is any other refusal of the request (bad HTML, too large).
	ErrRefused = errors.New("live write: refused")
)

// serviceNoSuchDoc is what the collaboration service's own refusal of a write
// to a doc it can't find says (live/ops/write.js): matched, never said to
// anyone, so it isn't a doc's 404 answer (helpers.DocNotFound).
const serviceNoSuchDoc = "no such doc"

// OpError is the service's answer to a write it did not make.
type OpError struct {
	Op     string
	Status int
	Msg    string
}

// Error is "live <op>: <status> <msg>", the service's message intact (A9's
// refusals carry JSON there).
func (e *OpError) Error() string {
	return fmt.Sprintf("live %s: %d %s", e.Op, e.Status, e.Msg)
}

// StatusCode is the HTTP status the service answered.
func (e *OpError) StatusCode() int { return e.Status }

// Is lets callers test for the kind of refusal with errors.Is.
func (e *OpError) Is(target error) bool {
	switch target {
	case ErrNoSuchDoc:
		return e.Status == http.StatusNotFound && strings.Contains(e.Msg, serviceNoSuchDoc)
	case ErrNoSuchBlock:
		return e.Status == http.StatusNotFound && !strings.Contains(e.Msg, serviceNoSuchDoc)
	case ErrHasContent:
		return e.Status == http.StatusConflict
	case ErrDocInTrash:
		return e.Status == http.StatusGone
	case ErrUnavailable:
		return e.Status >= 500
	case ErrRefused:
		return e.Status >= 400 && e.Status < 500
	}
	return false
}

// By is who a write is from, taken from server context, never from the client.
// For an agent, UserUUID is the person who asked it and AgentUUID the agent.
type By struct {
	UserUUID  string `json:"user_uuid,omitempty"`
	AgentUUID string `json:"agent_uuid,omitempty"`
}

// Options shape one write.
type Options struct {
	By By
	// AgentEdit marks the written content as an agent's edit: its range is
	// recorded beside the body (the doc's agentEdits map) for the web app to
	// mark, and the version kept first is an agent_run version.
	AgentEdit bool
	// RunID is the agent run the write belongs to, for Revert.
	RunID string
	// SnapshotFirst keeps a version of the doc as it was just before the write,
	// named SnapshotName when that is set. SnapshotReason is the version's
	// reason ("before_write" when empty; a restore says "manual").
	SnapshotFirst  bool
	SnapshotName   string
	SnapshotReason string
}

// Result is what a write did.
type Result struct {
	// Blocks is how many top-level blocks the write added or replaced.
	Blocks int `json:"blocks"`
	// Removed is how many it took away.
	Removed int `json:"removed"`
	// BlockIDs are the ids of the blocks it wrote, once blocks have ids.
	BlockIDs []string `json:"block_ids"`
	// EditID names the agent edit recorded beside the body, with AgentEdit.
	EditID string `json:"edit_id,omitempty"`
	// Lineage is the doc's lineage after the write.
	Lineage string `json:"lineage,omitempty"`
	// Persisted is whether the doc was stored with the write in it. When it is
	// false the write is in the live doc and its save is being retried.
	Persisted bool `json:"persisted"`
	// PersistedAt is when the doc was stored with the write in it.
	PersistedAt time.Time `json:"persisted_at"`
	// VersionID is the version kept first, with SnapshotFirst ("" when the doc
	// was empty, or the version could not be kept, which is logged).
	VersionID string `json:"version_id,omitempty"`
}

// Current is a doc as the live copy holds it now.
type Current struct {
	HTML    string `json:"html"`
	Hash    string `json:"hash"`
	Lineage string `json:"lineage"`
	// Open is whether anyone had it open.
	Open bool `json:"open"`
}

// Writer is the Live Doc Writer. html is the editor's HTML; the service turns it
// into blocks with the editor's own schema.
type Writer interface {
	// Append adds blocks at the end.
	Append(ctx context.Context, docUUID, html string, o Options) (Result, error)
	// InsertAfter adds blocks after the block with that id.
	InsertAfter(ctx context.Context, docUUID, blockID, html string, o Options) (Result, error)
	// ReplaceBlocks replaces the blocks with those ids; empty html removes them.
	ReplaceBlocks(ctx context.Context, docUUID string, blockIDs []string, html string, o Options) (Result, error)
	// ReplaceAll replaces the whole body, as a restore or an API write does.
	// Blocks that read the same at its start and end keep their identity.
	ReplaceAll(ctx context.Context, docUUID, html string, o Options) (Result, error)
	// Seed writes the first body of a doc nobody has written in yet (a
	// template, a duplicate, an import). A doc with content answers
	// ErrHasContent, so a seed is never written twice.
	Seed(ctx context.Context, docUUID, html string, o Options) (Result, error)
	// Reauth asks the service to check the open connections again, for the users
	// listed or for everyone when none are, so a revoked or downgraded person's
	// socket is closed or made read-only (I10): their editors reconnect and are
	// authorised afresh.
	Reauth(ctx context.Context, docUUID string, userUUIDs []string) error
	// ApplyProposal runs an edit proposal's pending step (prepare, apply,
	// insert below) on the live doc, each write as one transaction (A9's op,
	// collaboration-service live/ops/applyProposal.js).
	ApplyProposal(ctx context.Context, docUUID, proposalID string, hunkIDs []string, o Options) (Result, error)
}

// Version is a doc's body as it was just before a write, for the Snapshot
// Service to keep (Options.SnapshotFirst).
type Version struct {
	DocUUID string
	HTML    string
	Hash    string
	Lineage string
	By      By
	Agent   bool
	RunID   string
	Name    string
	Reason  string
}

// KeepVersion keeps a version taken before a write and answers its id. The
// Snapshot Service sets it from its init (business/Snapshot, which this package
// cannot import). Unset, a version is not kept and the log says so.
var KeepVersion func(ctx context.Context, v Version) (string, error)

// Live is the writer everything uses.
var Live Writer = liveClient{}

// LiveURLEnv names the internal address of the service's live port.
const LiveURLEnv = "COLLAB_LIVE_URL"

// defaultLiveURL is the service on the compose network (final-compose.yml and
// distribute-compose.yml set LIVE_PORT=1235).
const defaultLiveURL = "http://collaboration-service:1235"

// liveTimeout bounds one write: loading a large doc, the transaction and the
// save it waits for.
const liveTimeout = 45 * time.Second

// MaxLiveBytes is the largest request the service takes (live/server.js).
const MaxLiveBytes = 16 << 20

var liveHTTP = &http.Client{Timeout: liveTimeout}

func liveURL() string {
	if u := strings.TrimRight(strings.TrimSpace(os.Getenv(LiveURLEnv)), "/"); u != "" {
		return u
	}
	return defaultLiveURL
}

// liveClient is the client for the service's live port.
type liveClient struct{}

type wire struct {
	HTML          string          `json:"html,omitempty"`
	BlockID       string          `json:"block_id,omitempty"`
	BlockIDs      []string        `json:"block_ids,omitempty"`
	ProposalID    string          `json:"proposal_id,omitempty"`
	HunkIDs       []string        `json:"hunk_ids,omitempty"`
	UserUUIDs     []string        `json:"user_uuids,omitempty"`
	UserIDs       []string        `json:"user_ids,omitempty"`
	State         string          `json:"state,omitempty"`
	TaskChecks    map[string]bool `json:"task_checks,omitempty"`
	By            *By             `json:"by,omitempty"`
	AgentEdit     bool            `json:"agent_edit,omitempty"`
	RunID         string          `json:"run_id,omitempty"`
	SnapshotFirst bool            `json:"snapshot_first,omitempty"`
}

type writeAnswer struct {
	Blocks      int        `json:"blocks"`
	Removed     int        `json:"removed"`
	BlockIDs    []string   `json:"block_ids"`
	EditID      string     `json:"edit_id"`
	Lineage     string     `json:"lineage"`
	Persisted   bool       `json:"persisted"`
	PersistedAt *time.Time `json:"persisted_at"`
	Before      *struct {
		HTML    string `json:"html"`
		Hash    string `json:"hash"`
		Lineage string `json:"lineage"`
	} `json:"before"`
}

// call sends one op on a doc and decodes its data into out.
func call(ctx context.Context, docUUID, op string, body wire, out any) error {
	return callAt(ctx, "/live/docs/"+docUUID+"/"+op, op, body, out)
}

// callAt sends one op to its address on the live port and decodes its data
// into out.
func callAt(ctx context.Context, path, op string, body wire, out any) error {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SECRET"))
	if secret == "" {
		return fmt.Errorf("%w: INTERNAL_SECRET is not set on this server", ErrUnavailable)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(payload) > MaxLiveBytes {
		return &OpError{Op: op, Status: http.StatusRequestEntityTooLarge, Msg: "too large to write at once"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveURL()+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)
	resp, err := liveHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	var envelope struct {
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if resp.StatusCode != http.StatusOK {
		msg := envelope.Msg
		if msg == "" {
			msg = resp.Status
		}
		return &OpError{Op: op, Status: resp.StatusCode, Msg: msg}
	}
	if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

// write runs a content op and keeps the version taken before it.
func write(ctx context.Context, docUUID, op string, body wire, o Options) (Result, error) {
	by := o.By
	body.By = &by
	body.AgentEdit = o.AgentEdit && o.By.AgentUUID != ""
	body.RunID = o.RunID
	body.SnapshotFirst = o.SnapshotFirst
	var a writeAnswer
	if err := call(ctx, docUUID, op, body, &a); err != nil {
		return Result{}, err
	}
	r := Result{
		Blocks: a.Blocks, Removed: a.Removed, BlockIDs: a.BlockIDs, EditID: a.EditID,
		Lineage: a.Lineage, Persisted: a.Persisted,
	}
	if a.PersistedAt != nil {
		r.PersistedAt = *a.PersistedAt
	}
	if o.SnapshotFirst && a.Before != nil && strings.TrimSpace(a.Before.HTML) != "" {
		r.VersionID = keepBefore(ctx, Version{
			DocUUID: docUUID, HTML: a.Before.HTML, Hash: a.Before.Hash, Lineage: a.Before.Lineage,
			By: o.By, Agent: body.AgentEdit, RunID: o.RunID, Name: o.SnapshotName, Reason: o.SnapshotReason,
		})
	}
	return r, nil
}

// keepBefore keeps the version taken before a write. A version that cannot be
// kept does not undo the write, which is already in the live doc: it is logged.
func keepBefore(ctx context.Context, v Version) string {
	if KeepVersion == nil {
		helpers.LogErrorWithContext(ctx, "services/collab no version store is set; the version before a write to %s was not kept", v.DocUUID)
		return ""
	}
	id, err := KeepVersion(context.WithoutCancel(ctx), v)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "services/collab keeping the version before a write to %s failed: %v", v.DocUUID, err)
		return ""
	}
	return id
}

func (liveClient) Append(ctx context.Context, docUUID, html string, o Options) (Result, error) {
	return write(ctx, docUUID, "append", wire{HTML: html}, o)
}

func (liveClient) InsertAfter(ctx context.Context, docUUID, blockID, html string, o Options) (Result, error) {
	return write(ctx, docUUID, "insertAfter", wire{HTML: html, BlockID: blockID}, o)
}

func (liveClient) ReplaceBlocks(ctx context.Context, docUUID string, blockIDs []string, html string, o Options) (Result, error) {
	return write(ctx, docUUID, "replaceBlocks", wire{HTML: html, BlockIDs: blockIDs}, o)
}

func (liveClient) ReplaceAll(ctx context.Context, docUUID, html string, o Options) (Result, error) {
	return write(ctx, docUUID, "replaceAll", wire{HTML: html}, o)
}

func (liveClient) Seed(ctx context.Context, docUUID, html string, o Options) (Result, error) {
	return write(ctx, docUUID, "seed", wire{HTML: html}, o)
}

func (liveClient) Reauth(ctx context.Context, docUUID string, userUUIDs []string) error {
	return call(ctx, docUUID, "reauth", wire{UserUUIDs: userUUIDs}, nil)
}

func (liveClient) ApplyProposal(ctx context.Context, docUUID, proposalID string, hunkIDs []string, o Options) (Result, error) {
	return write(ctx, docUUID, "applyProposal", wire{ProposalID: proposalID, HunkIDs: hunkIDs}, o)
}

// ReauthBoard closes the live sockets on a board of the people named
// (everyone's when none are), so they reconnect and are authorised afresh, as
// Reauth does a doc's (I10). Like Read, it is not a Writer method; a test may
// swap it.
var ReauthBoard = func(ctx context.Context, boardUUID string, userUUIDs []string) error {
	return callAt(ctx, "/live/boards/"+boardUUID+"/reauth", "reauth", wire{UserUUIDs: userUUIDs}, nil)
}

// ClosePeople closes every live socket the people named have open, on every
// doc and board: a member's (their uuid) or a guest's (guest-<grant id>), so
// their tabs reconnect and are authorised afresh. Naming nobody closes
// nothing. A test may swap it.
var ClosePeople = func(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return callAt(ctx, "/live/people/close", "close", wire{UserIDs: ids}, nil)
}

// closeTimeout bounds one close: the change that asked it is made, and a
// socket the service can't be reached to close is asked again whenever it
// reconnects anyway.
const closeTimeout = 5 * time.Second

// ClosePeopleLater is ClosePeople in the background, so a change never waits
// on the collaboration service.
func ClosePeopleLater(ids ...string) {
	if len(ids) == 0 {
		return
	}
	ids = append([]string(nil), ids...)
	closeLater("collab.close-people", func(ctx context.Context) error { return ClosePeople(ctx, ids) })
}

// ReauthBoardLater is ReauthBoard in the background.
func ReauthBoardLater(boardUUID string, userUUIDs []string) {
	userUUIDs = append([]string(nil), userUUIDs...)
	closeLater("collab.board-reauth", func(ctx context.Context) error { return ReauthBoard(ctx, boardUUID, userUUIDs) })
}

// closeLater runs one close in the background, bounded by closeTimeout. With
// no live port configured there is nothing to close: a socket is authorised
// again whenever it reconnects anyway.
func closeLater(name string, op func(ctx context.Context) error) {
	helpers.GoSafeNamed(name, func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		switch err := op(ctx); {
		case err == nil:
		case errors.Is(err, ErrUnavailable):
			helpers.LogInfoWithContext(ctx, "services/collab %s: live sockets can't be closed (%v)", name, err)
		default:
			helpers.LogErrorWithContext(ctx, "services/collab %s err: %+v", name, err)
		}
	})
}

// Read is a doc as the live copy holds it now, unsaved typing included: what an
// export or a version taken on request reads (the service loads a doc nobody
// has open, and stores nothing). It is not a Writer method, so the writers
// other packages fake keep the day-0 contract's methods. A test may swap it.
var Read = func(ctx context.Context, docUUID string) (Current, error) {
	var c Current
	err := call(ctx, docUUID, "read", wire{}, &c)
	return c, err
}

// Render writes a saved Yjs state as HTML with the service's schema, and says
// whether the doc is open there now (the repair command, business/Doc
// docRepair.go). With no state it only answers open.
func Render(ctx context.Context, docUUID, stateB64 string) (html string, open bool, err error) {
	var out struct {
		HTML string `json:"html"`
		Open bool   `json:"open"`
	}
	err = call(ctx, docUUID, "render", wire{State: stateB64}, &out)
	return out.HTML, out.Open, err
}
