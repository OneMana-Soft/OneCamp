package business

// A repair an install runs to the end once, in the background, as a server
// starts.
//
// Each repair rewrites what an earlier release left wrong (search entries, a
// field in the graph) and is idempotent, so running it again changes nothing.
// When it has run to the end, its system_configs key records that it has,
// with what it did, and later starts skip it. Until then each start runs it in
// the background, retrying with backoff up to its tries in that process; the
// next start tries again. A startup job rather than an admin command, so
// every install is repaired by upgrading.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// Once is a repair an install runs to the end once: Key records that it has,
// Name names it in the logs, Tries bounds its retries in one process, and
// Repair does the work and answers what it did.
type Once[R fmt.Stringer] struct {
	Key    string
	Name   string
	Tries  int
	Repair func(context.Context) (R, error)
}

// The record as Run, Progress and SaveProgress read and write it. Seams for
// tests.
var (
	isDone      = configModel.IsDone
	writeRecord = configModel.UpsertConfig
	readRecord  = configModel.GetConfigByKey
)

// Run does the repair unless it has run to the end on this install, and then
// records that it has, with what it did. ran is whether it ran this time; a
// repair that failed part way is recorded as nothing, so it runs again.
func (o Once[R]) Run(ctx context.Context) (rep R, ran bool, err error) {
	if done, err := isDone(o.Key); err != nil || done {
		return rep, false, err
	}
	rep, err = o.Repair(ctx)
	if err != nil {
		return rep, true, err
	}
	return rep, true, writeRecord(o.Key, "done: "+rep.String())
}

// Start runs Run in the background until it succeeds, ctx ends, or the tries
// run out.
func (o Once[R]) Start(ctx context.Context) {
	helpers.RetryInBackground(ctx, o.Name,
		helpers.Retries{First: 30 * time.Second, Cap: 30 * time.Minute, Tries: o.Tries},
		func(ctx context.Context) error {
			rep, ran, err := o.Run(ctx)
			if err != nil {
				helpers.MessageLogs.ErrorLog.Printf("%s failed (will retry): %v (so far: %s)", o.Name, err, rep)
				return err
			}
			if ran {
				helpers.MessageLogs.InfoLog.Printf("%s: %s", o.Name, rep)
			}
			return nil
		})
}

// Progress is where a repair that goes a page at a time got to on an earlier
// run, as SaveProgress kept it under the repair's key, so a run goes on from
// there; the zero P when it has not run yet. A record it can't read is the
// zero P too: a repair is idempotent, so it starts again.
func Progress[P any](key string) (P, error) {
	var none P
	rec, err := readRecord(key)
	if errors.Is(err, sql.ErrNoRows) || err == nil && rec == nil {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	var p P
	if json.Unmarshal([]byte(rec.Value), &p) != nil {
		return none, nil
	}
	return p, nil
}

// SaveProgress keeps where a repair got to under its key, until Run records
// it done there.
func SaveProgress(key string, p any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeRecord(key, string(b))
}
