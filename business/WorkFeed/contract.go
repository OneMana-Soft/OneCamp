// Package business (WorkFeed) is the Work Change Log and its feed
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #88 and PJ2; A3 builds it):
// every change to work, numbered in its workspace in the same transaction as
// the change (task_changes, written by the Task Writer), and read back by
// Since for whoever may see it: the browser's Task Store (#89) bootstraps and
// catches up from it, and live updates, webhooks, search and history read it
// rather than being told separately.
//
// DAY 0. No change is logged yet, so Since answers none and keeps the cursor
// it was given, and Publish does nothing. Nothing calls either, so nothing
// changes by their existing.
package business

import (
	"context"
	"encoding/json"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Cursor is where a reader is in the log: opaque, from the last answer.
type Cursor string

// Entry is one change as the feed answers it: its number in the workspace,
// the thing it changed, the fields' new values, who made it and from where.
// A tombstone tells the reader a thing left their sight.
type Entry struct {
	Sequence     int64           `json:"sequence"`
	ResourceKind string          `json:"resource_kind"`
	ResourceUUID string          `json:"resource_uuid"`
	Diff         json.RawMessage `json:"diff,omitempty"`
	Actor        string          `json:"actor,omitempty"`
	Source       string          `json:"source,omitempty"`
	At           time.Time       `json:"at"`
	Tombstone    bool            `json:"tombstone,omitempty"`
}

// Feed reads the log for one reader and takes new entries.
type Feed interface {
	// Since is what changed after cursor that who may see, in order, at most
	// limit entries, and the cursor to ask from next.
	Since(ctx context.Context, who accessBusiness.Subject, cursor Cursor, limit int) ([]Entry, Cursor, error)
	// Publish tells open clients of entries just logged.
	Publish(ctx context.Context, entries []Entry)
}

// Changes is the feed everything reads.
var Changes Feed = noChanges{}

// noChanges is today: nothing is logged.
type noChanges struct{}

func (noChanges) Since(_ context.Context, _ accessBusiness.Subject, cursor Cursor, _ int) ([]Entry, Cursor, error) {
	return nil, cursor, nil
}

func (noChanges) Publish(context.Context, []Entry) {}
