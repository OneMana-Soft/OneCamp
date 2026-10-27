package business

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

type tally struct{ n int }

func (t tally) String() string { return fmt.Sprintf("%d fixed", t.n) }

// fakeRecord stands in for system_configs: what each key holds, and whether
// reading or writing it fails.
type fakeRecord struct {
	values  map[string]string
	readErr error
	markErr error
}

func useRecord(t *testing.T, r *fakeRecord) {
	t.Helper()
	oldDone, oldWrite, oldRead := isDone, writeRecord, readRecord
	isDone = func(key string) (bool, error) {
		if r.readErr != nil {
			return false, r.readErr
		}
		return strings.HasPrefix(r.values[key], "done"), nil
	}
	writeRecord = func(key, value string) error {
		if r.markErr != nil {
			return r.markErr
		}
		r.values[key] = value
		return nil
	}
	readRecord = func(key string) (*configModel.SystemConfig, error) {
		if r.readErr != nil {
			return nil, r.readErr
		}
		v, ok := r.values[key]
		if !ok {
			return nil, sql.ErrNoRows
		}
		return &configModel.SystemConfig{Key: key, Value: v}, nil
	}
	t.Cleanup(func() { isDone, writeRecord, readRecord = oldDone, oldWrite, oldRead })
}

func repairing(calls *int, n int, err error) Once[tally] {
	return Once[tally]{Key: "test_repair_v1", Name: "test.repair", Tries: 1,
		Repair: func(context.Context) (tally, error) {
			*calls++
			return tally{n}, err
		}}
}

func TestOnceRecordsWhatTheRepairDidAndRunsItOnce(t *testing.T) {
	r := &fakeRecord{values: map[string]string{}}
	useRecord(t, r)
	calls := 0
	o := repairing(&calls, 3, nil)

	rep, ran, err := o.Run(context.Background())
	if err != nil || !ran || rep.n != 3 {
		t.Fatalf("first run: %s, ran %v, %v; want 3 fixed, ran", rep, ran, err)
	}
	if got := r.values["test_repair_v1"]; got != "done: 3 fixed" {
		t.Errorf("the record says %q, want %q", got, "done: 3 fixed")
	}
	if rep, ran, err := o.Run(context.Background()); err != nil || ran || rep.n != 0 || calls != 1 {
		t.Errorf("a second start: %s, ran %v, %v, %d runs; want nothing run", rep, ran, err, calls)
	}
}

func TestOnceRunsAgainAfterAFailure(t *testing.T) {
	down := errors.New("search is down")
	r := &fakeRecord{values: map[string]string{}}
	useRecord(t, r)
	calls := 0

	rep, ran, err := repairing(&calls, 2, down).Run(context.Background())
	if !errors.Is(err, down) || !ran || rep.n != 2 {
		t.Fatalf("a failing repair: %s, ran %v, %v; want what it did so far and its error", rep, ran, err)
	}
	if v, ok := r.values["test_repair_v1"]; ok {
		t.Errorf("a failed repair was recorded as %q; the next try would skip it", v)
	}
	if _, ran, err := repairing(&calls, 2, nil).Run(context.Background()); err != nil || !ran || calls != 2 {
		t.Errorf("the next try: ran %v, %v, %d runs; want it to run again", ran, err, calls)
	}
}

func TestOnceAnswersWhenItCantReadOrWriteItsRecord(t *testing.T) {
	unreadable := errors.New("no database")
	calls := 0
	useRecord(t, &fakeRecord{values: map[string]string{}, readErr: unreadable})
	if _, ran, err := repairing(&calls, 1, nil).Run(context.Background()); !errors.Is(err, unreadable) || ran || calls != 0 {
		t.Errorf("with its record unreadable: ran %v, %v, %d runs; want the error and nothing run", ran, err, calls)
	}

	unwritable := errors.New("read-only")
	useRecord(t, &fakeRecord{values: map[string]string{}, markErr: unwritable})
	if _, ran, err := repairing(&calls, 1, nil).Run(context.Background()); !errors.Is(err, unwritable) || !ran {
		t.Errorf("with its record unwritable: ran %v, %v; want the error, so the start tries again", ran, err)
	}
}

type place struct {
	After string `json:"after"`
	Done  int    `json:"done"`
}

// A repair that goes a page at a time keeps where it got to under its key,
// and a later run reads it back: nothing before it ever ran, and nothing from
// a record it can't read, since a repair starts again safely. Run's "done"
// then writes over it.
func TestProgressIsWhereTheLastRunGotTo(t *testing.T) {
	r := &fakeRecord{values: map[string]string{}}
	useRecord(t, r)
	if p, err := Progress[place]("test_repair_v1"); err != nil || p != (place{}) {
		t.Fatalf("never run: %+v, %v", p, err)
	}
	if err := SaveProgress("test_repair_v1", place{After: "b", Done: 2}); err != nil {
		t.Fatal(err)
	}
	if p, err := Progress[place]("test_repair_v1"); err != nil || p != (place{After: "b", Done: 2}) {
		t.Fatalf("after a run: %+v, %v", p, err)
	}
	r.values["test_repair_v1"] = "{not json"
	if p, err := Progress[place]("test_repair_v1"); err != nil || p != (place{}) {
		t.Fatalf("unreadable: %+v, %v", p, err)
	}
	calls := 0
	if _, _, err := repairing(&calls, 1, nil).Run(context.Background()); err != nil || r.values["test_repair_v1"] != "done: 1 fixed" {
		t.Fatalf("the run's end: %q, %v", r.values["test_repair_v1"], err)
	}
	r.readErr = errors.New("down")
	if _, err := Progress[place]("test_repair_v1"); !errors.Is(err, r.readErr) {
		t.Fatalf("a failed read: %v", err)
	}
}
