package models

// Receipts for the evidence pack: what it said, at the time it said it.
//
// See migration 159 for why this exists. In short: a pack is assembled from rows
// that still exist, retention redacts row content once it passes the window, and
// so a pack built later over the same window is a weaker document than one built
// at the time. The receipt is taken while the rows are intact and stores no
// content, only the fingerprint, the manifest digests and the verification
// counts, which is enough to make a later regeneration checkable.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// EvidenceReceipt is one window's record.
type EvidenceReceipt struct {
	ID              uuid.UUID       `json:"id"`
	PeriodStart     time.Time       `json:"period_start"`
	PeriodEnd       time.Time       `json:"period_end"`
	GeneratedAt     time.Time       `json:"generated_at"`
	PackFingerprint string          `json:"pack_fingerprint"`
	Manifest        json.RawMessage `json:"manifest"`
	ChainOK         bool            `json:"chain_ok"`
	ChainChecked    int             `json:"chain_checked"`
	ChainRedacted   int             `json:"chain_redacted"`
}

const receiptColumns = `id, period_start, period_end, generated_at, pack_fingerprint,
	manifest, chain_ok, chain_checked, chain_redacted`

// InsertReceiptIfAbsent records a receipt for a window, and reports whether it
// wrote one.
//
// DO NOTHING rather than an upsert, and that is the whole point of the table.
// The job that calls this runs on a timer and retries; an upsert would let a
// retry overwrite a receipt taken while the rows were intact with one taken
// after retention had redacted them, which is precisely the substitution the
// receipt exists to prevent. The first answer for a window is the one that
// counts, and a later regeneration that disagrees is a finding, not a correction.
func InsertReceiptIfAbsent(ctx context.Context, r *EvidenceReceipt) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	manifest := r.Manifest
	if len(manifest) == 0 {
		manifest = json.RawMessage(`[]`)
	}

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `
		INSERT INTO evidence_receipts
			(period_start, period_end, generated_at, pack_fingerprint, manifest,
			 chain_ok, chain_checked, chain_redacted)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (period_start, period_end) DO NOTHING`,
		r.PeriodStart, r.PeriodEnd, r.GeneratedAt, r.PackFingerprint, []byte(manifest),
		r.ChainOK, r.ChainChecked, r.ChainRedacted)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// HasReceiptFor reports whether a window already has one, so the job can decide
// without building a pack it is going to throw away. Building one is a full
// chain verification and an export; asking first is an index lookup.
func HasReceiptFor(ctx context.Context, from, to time.Time) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var exists bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT EXISTS (SELECT 1 FROM evidence_receipts WHERE period_start = $1 AND period_end = $2)`,
		from, to).Scan(&exists)
	return exists, err
}

// ListReceipts returns the newest windows first, which is the only order anyone
// reads them in.
func ListReceipts(ctx context.Context, limit int) ([]*EvidenceReceipt, error) {
	if limit <= 0 || limit > 200 {
		limit = 24
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT `+receiptColumns+` FROM evidence_receipts ORDER BY period_start DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*EvidenceReceipt{}
	for rows.Next() {
		var r EvidenceReceipt
		var manifest []byte
		if err := rows.Scan(&r.ID, &r.PeriodStart, &r.PeriodEnd, &r.GeneratedAt,
			&r.PackFingerprint, &manifest, &r.ChainOK, &r.ChainChecked, &r.ChainRedacted); err != nil {
			return nil, err
		}
		r.Manifest = json.RawMessage(manifest)
		out = append(out, &r)
	}
	return out, rows.Err()
}
