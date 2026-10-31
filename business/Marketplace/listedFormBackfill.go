package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	"github.com/google/uuid"
)

// listedFormBackfillTries bounds the retries in one process (Postgres may
// still be starting); the next start tries again.
const listedFormBackfillTries = 8

// listedFormBatch is how many templates the backfill reads at a time: each
// may carry a payload of up to a megabyte.
const listedFormBatch = 20

// StartListedFormBackfill keeps the listed form of every template saved
// before it was kept, so the Templates list never reads a payload (audit
// C11). Until it has, the list makes a missing form from the payload.
func StartListedFormBackfill(ctx context.Context) {
	helpers.RetryInBackground(ctx, "marketplace.listed-form-backfill",
		helpers.Retries{First: 10 * time.Second, Cap: 10 * time.Minute, Tries: listedFormBackfillTries},
		func(ctx context.Context) error {
			kept, err := backfillListedForms(ctx)
			if kept > 0 || err != nil {
				helpers.MessageLogs.InfoLog.Printf("templates: kept %d listed forms (err: %v)", kept, err)
			}
			return err
		})
}

// backfillListedForms keeps the missing listed forms, a batch at a time in id
// order, and moves past a batch it can't write: the list still makes those
// from their payloads, and the next start tries them again.
func backfillListedForms(ctx context.Context) (int, error) {
	kept := 0
	after := uuid.Nil
	for {
		batch, err := model.UnlistedAfter(ctx, after, listedFormBatch)
		if err != nil || len(batch) == 0 {
			return kept, err
		}
		ids := make([]uuid.UUID, len(batch))
		forms := make([]string, len(batch))
		for i, t := range batch {
			ids[i], forms[i] = t.Id, listedForm(t.Kind, t.Payload)
		}
		after = ids[len(ids)-1]
		if err := model.KeepListedPayloads(ctx, ids, forms); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Marketplace listed forms after %s err: %+v", ids[0], err)
			continue
		}
		kept += len(batch)
	}
}
