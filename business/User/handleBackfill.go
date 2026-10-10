package business

// Handles for the members who joined before there were handles.
//
// Handles (users.username) arrived in v2.70.0 and are given at sign up, so
// every account from before has none, and the profile editor showed an empty
// @handle. At startup, in the background, each member without one is given
// the handle they would have had on joining: derived from their display name,
// else their full name, else their address (ensureHandle, with the same -2,
// -3 for a handle taken and a retry when another join takes it first), in
// the order they joined: of two Sams without one, the first to join is @sam.
//
// It runs at every start rather than once per install behind a flag in
// system_configs. When nobody is left, a run is one query that finds no rows.
// A flag would stop it for good after one pass, and members still arrive
// without a handle afterwards: the demo's visitor (EnsureDemoUserExists) and
// anyone restored from a backup taken before v2.70.0; and a member whose
// handle couldn't be set on the first pass would be left without one.
// Bots and agents are never touched: they keep their own names
// (agentBot.go, automationBot.go).

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

const (
	handleBackfillPage = 200
	// handleBackfillTries bounds the retries in one process (Dgraph or
	// Postgres may still be starting); the next start tries again.
	handleBackfillTries = 8
)

// StartHandleBackfill runs BackfillHandles in the background until a run
// finishes, ctx ends, or the tries run out. It never delays or fails a start.
func StartHandleBackfill(ctx context.Context) {
	helpers.GoSafeNamed("user.handle-backfill", func() {
		wait := 10 * time.Second
		for try := 0; try < handleBackfillTries; try++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			given, failed, err := BackfillHandles(ctx)
			if given > 0 || failed > 0 || err != nil {
				helpers.MessageLogs.InfoLog.Printf("handles: gave %d members without a handle one, %d could not be given one (err: %v)", given, failed, err)
			}
			if err == nil {
				return
			}
			if wait < 10*time.Minute {
				wait *= 2
			}
		}
	})
}

// HandleBackfillLockKey is the Postgres advisory lock a run holds, so that of
// the replicas starting together one gives the handles and the rest skip the
// run. It is "Handles" in ASCII: outside the 32-bit range of the hashtext()
// keys taken elsewhere (bookings, cycles, the goal tree) and of
// golang-migrate's lock, odd where sqlx migrate's is always even, and not the
// admin audit chain's 472074.
const HandleBackfillLockKey int64 = 0x48616e646c6573

// BackfillHandles gives every member without a handle one, a page at a time,
// and answers how many it gave and how many it could not. An error means a
// page could not be read; the members before it have their handles. A run
// holds HandleBackfillLockKey on a connection of its own; while another
// replica's run holds it, this one gives nobody a handle and answers 0, 0,
// nil.
func BackfillHandles(ctx context.Context) (given, failed int, err error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	conn, err := postgresInit.DBConn.SqlDB.Conn(c)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	var mine bool
	if err := conn.QueryRowContext(c, `SELECT pg_try_advisory_lock($1)`, HandleBackfillLockKey).Scan(&mine); err != nil {
		return 0, 0, err
	}
	if !mine {
		return 0, 0, nil
	}
	defer func() {
		// The lock is the session's, so it is released before the
		// connection goes back to the pool, even once ctx has ended: a
		// pooled connection still holding it would keep every replica from
		// running this until the pool closed it.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), postgresInit.DBConn.DBTimeout)
		defer cancel()
		if _, err := conn.ExecContext(c, `SELECT pg_advisory_unlock($1)`, HandleBackfillLockKey); err != nil {
			helpers.LogWarnWithContext(ctx, "business/BackfillHandles releasing its lock: %+v", err)
		}
	}()

	var after domain.MemberWithoutHandle // the zero value: from the first to join
	for {
		page, err := domain.MembersWithoutHandle(ctx, after, handleBackfillPage)
		if err != nil {
			return given, failed, err
		}
		if len(page) == 0 {
			return given, failed, nil
		}
		ids := make([]string, len(page))
		for i, m := range page {
			ids[i] = m.ID.String()
		}
		// Names live in the graph. Without them a handle would be made from
		// the address when the person has a name, so a failed read waits
		// for the next try rather than settle for that.
		names, err := domain.GetUserDisplayMapByUUIDs(ctx, ids)
		if err != nil {
			return given, failed, err
		}
		for _, m := range page {
			name := ""
			if u := names[m.ID.String()]; u != nil {
				name = u.DisplayName()
			}
			if ensureHandle(ctx, m.ID, name, m.Email) != "" {
				given++
			} else {
				failed++
			}
		}
		after = page[len(page)-1]
	}
}
