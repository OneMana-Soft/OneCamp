package models

// The admin's sync ledger (gcal-8 H1): every Google and Outlook link in the
// workspace with its counts, in one statement, and an admin's break of one.
// The text is domain/CalendarV2's (syncLedgerDomain.go).

import (
	"context"

	calendarV2Domain "github.com/akashc777/OneCamp/domain/CalendarV2"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// SyncLedgerRow is one link of the ledger: the link, how many of its
// person's changes the provider refused (calendar_sync_refusals), and how
// many of their calendar.sync outbox entries died.
type SyncLedgerRow struct {
	Link      SyncLink
	Refusals  int
	DeadSends int
}

// SyncLedgerFilter is a page of the ledger: a state and a provider ("" for
// any), and Limit rows after Offset.
type SyncLedgerFilter struct {
	State, Provider string
	Limit, Offset   int
}

func scanSyncLedgerRow(s pgexec.Row) (SyncLedgerRow, error) {
	var r SyncLedgerRow
	err := s.Scan(syncLinkFields(&r.Link, &r.Refusals, &r.DeadSends)...)
	return r, err
}

// SyncLedger is a page of the workspace's Google and Outlook links with their
// counts, in one statement.
func SyncLedger(ctx context.Context, f SyncLedgerFilter) ([]SyncLedgerRow, error) {
	out, err := pgexec.Query(ctx, calendarV2Domain.SyncLedger, scanSyncLedgerRow, f.State, f.Provider, f.Limit, f.Offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncLedger err: %+v", err)
	}
	return out, err
}

// BreakSyncLinkByAdmin breaks a Google or Outlook link, whatever its state,
// with why as its person reads it, and answers it with whether an admin had
// broken it already (a break sent again); nil for an id no such link has.
func BreakSyncLinkByAdmin(ctx context.Context, id uuid.UUID, why string) (*SyncLink, bool, error) {
	var again bool
	l, _, err := pgexec.QueryOne(ctx, calendarV2Domain.BreakSyncLinkByAdmin, func(s pgexec.Row) (*SyncLink, error) {
		var l SyncLink
		return &l, s.Scan(syncLinkFields(&l, &again)...)
	}, id, why)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.BreakSyncLinkByAdmin err: %+v", err)
		return nil, false, err
	}
	return l, again, nil
}
