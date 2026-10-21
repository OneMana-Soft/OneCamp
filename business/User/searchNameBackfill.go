package business

// Search names for the people saved before there were search names.
//
// People searches start from user_search_name (domain/User searchName.go),
// which every save writes. Everyone saved before it existed has none, and a
// search by a term long enough for its index can't find them. At start, in
// the background, each of them is given theirs (domain.BackfillSearchNames).
//
// It runs at every start rather than once per install behind a flag: when
// everyone's is right, a run reads each person once and writes nothing, and
// people restored from a backup taken before it existed get theirs on the
// next start. Replicas starting together are safe without a lock: each page
// is read and written in one transaction, so two runs over the same people
// abort one another's page and the one that runs again finds nothing to do.

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// searchNameBackfillTries bounds the retries in one process (Dgraph may
// still be starting); the next start tries again.
const searchNameBackfillTries = 8

// StartSearchNameBackfill runs domain.BackfillSearchNames in the background
// until a run finishes, ctx ends, or the tries run out. It never delays or
// fails a start.
func StartSearchNameBackfill(ctx context.Context) {
	helpers.RetryInBackground(ctx, "user.search-name-backfill",
		helpers.Retries{First: 10 * time.Second, Cap: 10 * time.Minute, Tries: searchNameBackfillTries},
		func(ctx context.Context) error {
			written, err := domain.BackfillSearchNames(ctx)
			if written > 0 || err != nil {
				helpers.MessageLogs.InfoLog.Printf("search names: wrote %d people's (err: %v)", written, err)
			}
			return err
		})
}
