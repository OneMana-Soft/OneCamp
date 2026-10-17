package models

// The chain's one writer: every audit row this process writes joins the chain
// in the order it was handed over.
//
// THE BUG THIS FIXES. Record (business/AdminAudit) wrote each row from a
// goroutine of its own, and a row's time and its place in the chain were given
// when that goroutine took the chain's lock. Two saves a moment apart raced for
// the lock, and the second could be recorded first: an allow-list changed from
// A to B read, in the log, as B then A, with the later time on the earlier
// change. A record that disagrees with what happened is a wrong record.
//
// WHY A QUEUE AND NOT A WRITE IN THE REQUEST. Measured on 15 Oct 2026 against
// the test Postgres: a row written while the request waits costs a commit, and
// a commit waits for the disk (p50 14 ms, p99 119 ms, where a round trip is
// 0.45 ms); eight requests at once queue on the chain's lock (p50 106 ms, p99
// 518 ms). A commit that doesn't wait for the disk still queued on the lock
// (p50 54 ms with eight). Handing a row over costs the request a mutex and an
// append, and one commit carries every row queued behind it. Audit writes also
// hold one pooled connection between them, where a goroutine per row held one
// each while it waited for the lock, and a burst could take the pool (10).
//
// WHAT ORDER MEANS HERE. Each row is stamped when it is handed over, under the
// queue's lock, and the stamps never go backwards (a clock stepped back repeats
// the last one), so in a process the chain's order, the times' order and the
// calls' order are one order. Across processes the advisory lock still makes
// one linear chain, which Verify checks, and two processes' rows interleave a
// batch at a time.
//
// WHO WAITS. Append is for a row nobody waits for (an admin's change, already
// made and on screen). Insert waits until its row is on disk and says whether
// it is, for "no record, no action" (RecordForPrincipal). They share the queue,
// so neither overtakes the other. helpers.SettleBackground waits until what was
// handed over is written: a server shutting down, and tests that read the log.
//
// An entry handed over belongs to the writer, which sets its id, time, place
// and hashes; the caller doesn't touch it again (Insert's caller may read it
// once Insert returns).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// chainBatchMax is the most rows one transaction writes: enough to carry a
// burst on one commit, few enough that the chain's lock, which every process
// waits on, is held briefly.
const chainBatchMax = 25

// chainQueueMax bounds what waits in memory. It is reached only when rows
// can't be written for a long while and the actions they record still
// succeed; past it a row is refused, and said so, rather than memory growing
// without end.
const chainQueueMax = 10000

// ErrAuditBacklog is a row refused because too many are waiting.
var ErrAuditBacklog = errors.New("audit: too many rows are waiting to be written")

type queuedEntry struct {
	e *AuditEntry
	// done receives the row's outcome. nil when nobody waits (Append).
	done chan error
}

type chainWriter struct {
	mu       sync.Mutex
	queue    []queuedEntry
	inflight helpers.Inflight // handed over, not yet written or failed
	wake     chan struct{}
	last     time.Time // the latest stamp given
	start    sync.Once
}

var chain = &chainWriter{wake: make(chan struct{}, 1)}

// The rows handed over and not yet written are background work: a server
// shutting down and a test reading the log wait for them through
// helpers.SettleBackground.
func init() { helpers.RegisterBackground("audit rows", chain.inflight.Settle) }

// Append hands e to the chain's writer and returns at once. e joins the chain
// after every row handed over before it in this process. A row that can't be
// written is logged with its action.
func Append(e *AuditEntry) {
	if err := chain.hand(e, nil); err != nil {
		helpers.LogErrorWithContext(context.Background(), "models/AdminAudit.Append %s: %v", e.Action, err)
	}
}

// Insert hands e to the chain's writer and waits until it is written,
// returning the write's error. It waits for the outcome even when ctx ends: a
// row handed over is written or fails on its own terms, and the answer has to
// say which, since a caller that refuses an action it can't record relies on
// it. Every batch has the database timeout, so the wait ends once the rows
// ahead of it are written or have failed.
func Insert(ctx context.Context, e *AuditEntry) error {
	done := make(chan error, 1)
	if err := chain.hand(e, done); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Insert %s: %v", e.Action, err)
		return err
	}
	return <-done
}

// hand stamps e and queues it.
func (w *chainWriter) hand(e *AuditEntry, done chan error) error {
	w.start.Do(func() { go w.run() })
	w.mu.Lock()
	if len(w.queue) >= chainQueueMax {
		w.mu.Unlock()
		return ErrAuditBacklog
	}
	if e.Id == uuid.Nil {
		e.Id = uuid.New()
	}
	// Microseconds, as Postgres keeps them: the hash is of what the row holds.
	at := time.Now().UTC().Truncate(time.Microsecond)
	if at.Before(w.last) {
		at = w.last
	}
	w.last = at
	e.CreatedAt = at
	w.queue = append(w.queue, queuedEntry{e: e, done: done})
	w.inflight.Add(1)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// run writes the queue out, a batch at a time, for the life of the process.
func (w *chainWriter) run() {
	for range w.wake {
		for {
			w.mu.Lock()
			n := min(len(w.queue), chainBatchMax)
			batch := append([]queuedEntry(nil), w.queue[:n]...)
			clear(w.queue[:n]) // the queue's array holds no row once it is taken
			w.queue = w.queue[n:]
			w.mu.Unlock()
			if n == 0 {
				break
			}
			writeOut(batch)
			w.inflight.Add(-n)
		}
	}
}

// writeOut writes a batch and gives each row its outcome. A row the database
// refuses for what it holds fails alone and the rest are written without it;
// any other failure fails the rows not yet written.
func writeOut(batch []queuedEntry) {
	for len(batch) > 0 {
		bad, err := writeBatch(batch)
		switch {
		case err == nil:
			for _, q := range batch {
				q.finish(nil)
			}
			return
		case bad < 0:
			for _, q := range batch {
				q.finish(err)
			}
			return
		}
		batch[bad].finish(err)
		batch = append(batch[:bad:bad], batch[bad+1:]...)
	}
}

func (q queuedEntry) finish(err error) {
	if q.done != nil {
		q.done <- err
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(context.Background(), "models/AdminAudit: the %s row was not written: %v", q.e.Action, err)
	}
}

// writeBatch links the batch into the chain, in order, in one transaction
// under the chain's lock. bad is the index of the row the database refused for
// what it holds, or -1 when the failure was not one row's.
func writeBatch(batch []queuedEntry) (bad int, err error) {
	defer func() {
		// The writer outlives every batch: a panic fails this one.
		if p := recover(); p != nil {
			bad, err = -1, fmt.Errorf("audit: writing a batch panicked: %v", p)
		}
	}()
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return -1, errors.New("audit: no database connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return -1, fmt.Errorf("audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// One linear chain across every process on this database.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, auditChainLockKey); err != nil {
		return -1, fmt.Errorf("audit: lock: %w", err)
	}
	var head sql.NullString
	if err = tx.QueryRowContext(ctx,
		`SELECT entry_hash FROM admin_audit_log WHERE entry_hash IS NOT NULL ORDER BY seq DESC LIMIT 1`).Scan(&head); err != nil && err != sql.ErrNoRows {
		return -1, fmt.Errorf("audit: the chain's head: %w", err)
	}

	const q = `
		INSERT INTO admin_audit_log
			(id, actor_id, actor_email, action, category, summary, metadata,
			 ip_address, user_agent, created_at, entry_hash, prev_hash, actor_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING seq`
	prev := head.String
	for i, item := range batch {
		e := item.e
		e.PrevHash = prev
		e.EntryHash = computeAuditHash(prev, e)
		if err = tx.QueryRowContext(ctx, q,
			e.Id, e.ActorID, nullStr(e.ActorEmail), e.Action, e.Category, e.Summary,
			e.Metadata, nullStr(e.IPAddress), nullStr(e.UserAgent), e.CreatedAt,
			e.EntryHash, nullStr(e.PrevHash), nullStr(e.ActorKind)).Scan(&e.Seq); err != nil {
			if helpers.IsRowRejected(err) {
				return i, fmt.Errorf("audit: the row was refused: %w", err)
			}
			return -1, fmt.Errorf("audit: insert: %w", err)
		}
		prev = e.EntryHash
	}
	if err = tx.Commit(); err != nil {
		return -1, fmt.Errorf("audit: commit: %w", err)
	}
	return -1, nil
}
