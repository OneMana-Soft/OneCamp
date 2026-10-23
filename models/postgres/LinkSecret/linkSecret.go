package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// clearLinks are the columns that kept a link's secret in the clear before
// audit A1, each beside the column its hash is now found by, as the
// migrations that added the hashes name them.
var clearLinks = []struct{ table, raw, hash string }{
	{"bookings", "cancel_token", "cancel_token_hash"},
	{"invitations", "token", "token_hash"},
	{"webhooks", "token", "token_hash"},
}

// HashLeftInClear gives every link still kept in the clear its hash and
// takes it out of the clear. Each hash is helpers.LinkSecretHash, worked out
// by Postgres's sha256() over the link's text, as the migrations work it out.
//
// The migrations only add the hash, so a server from before them keeps
// working until this one starts; this, at the start, catches what it wrote in
// between: a link with no hash, or one it changed after its hash was worked
// out. Once nothing is left in the clear it changes nothing. Idempotent. A
// table that fails doesn't keep the others in the clear: each is tried, and
// what failed comes back together.
func HashLeftInClear(ctx context.Context) error {
	var failed []error
	for _, c := range clearLinks {
		if _, err := pgexec.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s SET %[3]s = sha256(convert_to(%[2]s, 'UTF8')), %[2]s = NULL
			WHERE %[2]s IS NOT NULL`, c.table, c.raw, c.hash)); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", c.table, err))
		}
	}
	return errors.Join(failed...)
}

// startRetries is how HashLeftInClearAtStart tries again: from 30 s, doubling
// to every 15 min, for about a day.
var startRetries = helpers.Retries{First: 30 * time.Second, Cap: 15 * time.Minute, Tries: 100}

// HashLeftInClearAtStart runs HashLeftInClear before the server serves, and
// when that fails, again in the background until it works: until then a link
// the old server replaced (an invitation sent again, a webhook's token made
// anew) still works, so the start never leaves it to the next one. The "links" system check says so
// meanwhile.
func HashLeftInClearAtStart(ctx context.Context) {
	try := func(ctx context.Context) error {
		err := HashLeftInClear(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "links kept in the clear couldn't all be hashed, and are tried again: %v", err)
		}
		return err
	}
	if try(ctx) != nil {
		helpers.RetryInBackground(ctx, "links.hash-left-in-clear", startRetries, try)
	}
}

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "links",
		Describe: "No booking or invitation link, and no incoming webhook's token, is kept in the clear, only its " +
			"hash, so a copy of the database opens nothing and a link that was replaced stops working. It does not " +
			"check links already mailed.",
		Probe: inClear,
	})
}

// inClear is the "links" check: an error naming the tables that still keep
// a link in the clear, nil when none does. Read-only.
func inClear(ctx context.Context) error {
	var tables []string
	for _, c := range clearLinks {
		left, err := pgexec.Scalar[bool](ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s IS NOT NULL)`, c.table, c.raw))
		if err != nil {
			return fmt.Errorf("%s: %w", c.table, err)
		}
		if left {
			tables = append(tables, c.table)
		}
	}
	if len(tables) == 0 {
		return nil
	}
	return fmt.Errorf("links are still kept in the clear in %s, and a link that was replaced there still works. "+
		"The server hashes them at its start and keeps trying; if this stays, its log says why. Restarting it tries again",
		strings.Join(tables, " and "))
}
