package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// ErrNotRecorded is a change refused because its audit row couldn't be
// written: no record, no action.
var ErrNotRecorded = errors.New("audit: the change couldn't be recorded, so it wasn't made")

// recordedSeams are where Recorded and RecordedAs write: seams for tests.
var recordedSeams = struct {
	written func(r *http.Request, action, category, summary string, metadata map[string]interface{}) error
	record  func(r *http.Request, action, category, summary string, metadata map[string]interface{})
	as      func(ctx context.Context, actorID *uuid.UUID, actorEmail, actorKind, action, category, summary string, metadata map[string]interface{}) error
}{written: RecordWritten, record: Record, as: RecordForPrincipal}

// RecordedAs is Recorded where the change runs with no request to read (a
// business path that several routes share, an agent's call): its rows name p.
// The row is written before change runs; if it can't be, change never runs
// and the answer wraps ErrNotRecorded. If change then fails, an
// action+"_failed" row with the same metadata says it didn't take (logged if
// that one can't be written), and the answer is change's error.
func RecordedAs(ctx context.Context, p Principal, action, category, summary string, metadata map[string]interface{}, change func() error) error {
	if err := recordedSeams.as(ctx, p.ID, p.Email, p.Kind, action, category, summary, metadata); err != nil {
		return fmt.Errorf("%w: %v", ErrNotRecorded, err)
	}
	if err := change(); err != nil {
		if ferr := recordedSeams.as(ctx, p.ID, p.Email, p.Kind, action+"_failed", category, summary+" (it didn't take)", metadata); ferr != nil {
			helpers.LogErrorWithContext(ctx, "audit: %s_failed wasn't recorded: %+v", action, ferr)
		}
		return err
	}
	return nil
}

// Recorded makes a change only once its audit row is written: no record, no
// action. It is for a change whose row is the lasting record of who made it
// and when, such as a team's member or admin taken off, which leaves no node or
// row behind. If the row can't be written, change never runs and the answer
// wraps ErrNotRecorded. If change then fails, a second row,
// action+"_failed" with the same metadata, says the recorded change didn't
// take, and the answer is change's error.
func Recorded(r *http.Request, action, category, summary string, metadata map[string]interface{}, change func() error) error {
	if err := recordedSeams.written(r, action, category, summary, metadata); err != nil {
		return fmt.Errorf("%w: %v", ErrNotRecorded, err)
	}
	if err := change(); err != nil {
		recordedSeams.record(r, action+"_failed", category, summary+" (it didn't take)", metadata)
		return err
	}
	return nil
}
