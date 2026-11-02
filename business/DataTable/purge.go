package business

// A table deleted for good (V-121; PROGRAM.md §13 O3): from the trash by its
// owner, someone with full access or an admin (docs' own rule,
// accessBusiness.PurgeNeeds), or by the trash's sweep past its retention.
// What the table holds outside its own store goes first, each through its
// owner: its rows' pages (their guest links, then docs' purge), their
// comment threads, follows and notices, its files (archived, so the
// attachments purge takes the bytes), its guest links and everyone's places
// on it. Then its rows, a chunk at a
// time, and the table. Every step finds only what is left, and the table
// stays claimed until it's gone, so the sweep finishes a delete cut short.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	threadModels "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

// The owners' paths for what a table holds outside its own store (seams for
// tests).
var (
	// purgeHostedDocs is docs' purge for the pages a table's rows host.
	purgeHostedDocs   = docBusiness.PurgeHostedDocs
	forgetThreads     = threadModels.ForgetResources
	forgetFollows     = followModels.ForgetResources
	archivePlaceFiles = attachmentModels.ArchivePlaces
	forgetGuestLinks  = guestBusiness.ForgetGrantsOf
	forgetPlaces      = placesBusiness.Forget
)

// hostedPagesPerPass is how many of the tables' row pages one pass of a
// delete for good hands docs' purge: docs' limit on one trash operation (a
// var, so a test can make a table hold more than one pass takes).
var hostedPagesPerPass = docBusiness.TrashOperationCap

// claimStalePurge is the trash's sweep's one read at any retention, 0 (keep)
// included: a delete for good a stopped server left part way (a seam, so a
// unit test runs the sweep with no database).
var claimStalePurge = model.ClaimStalePurge

// Why a table went for good, as its table.purged event says.
const (
	purgedForGood   = "deleted for good"
	purgedRetention = "retention"
)

const (
	// trashPurgeBatch is how many tables past their retention one pass of the
	// trash's sweep deletes for good; the next hourly pass takes more.
	trashPurgeBatch = 4
	// purgeStale is how long a delete for good may run before the sweep takes
	// it for one a stopped server left part way, and finishes it.
	purgeStale = 30 * time.Minute
)

// errNotInTrash is a delete for good of a table that isn't in the trash.
var errNotInTrash = fmt.Errorf("Only a table in the trash can be deleted for good")

// errPagesLeft is a pass that handed docs as many of the tables' row pages
// as one pass may: the tables stay claimed, and the trash's sweep takes the
// next pages in its next pass, until the last is gone.
var errPagesLeft = errors.New("more of the tables' row pages are left for the trash's next pass")

// purging counts the deletes for good still running after their request
// answered: a server shutting down, and a test, wait for them.
var purging helpers.Inflight

func init() { helpers.RegisterBackground("tables' deletes for good", purging.Settle) }

// PurgeTable deletes a table in the trash for good, for its owner, someone
// with full access or an admin, as docs decide (accessBusiness.OpenDeleted,
// PurgeNeeds): one they never saw is not found, one they could only edit is
// refused, and a live one is a conflict (only the trash deletes for good).
// The table leaves the trash at once; what it holds goes after the request
// answers.
func PurgeTable(ctx context.Context, id uuid.UUID, who accessBusiness.Subject, actor Actor) error {
	res := accessBusiness.Resource{Kind: tableAccess.KindTable, UUID: id.String()}
	if _, err := accessBusiness.OpenDeleted(ctx, who, res, accessBusiness.PurgeNeeds); err != nil {
		switch {
		case errors.Is(err, accessBusiness.ErrNotFound):
			return errNotFound
		case errors.Is(err, accessBusiness.ErrNotDeleted):
			return errNotInTrash
		case errors.Is(err, accessBusiness.ErrForbidden):
			return errForbidden
		}
		return fmt.Errorf("failed to decide who may delete the table: %w", err)
	}
	claimed, err := model.ClaimTableForPurge(ctx, id, actor.UserID, time.Now())
	if err != nil {
		return fmt.Errorf("failed to delete the table for good")
	}
	if !claimed {
		return errNotFound
	}
	later := context.WithoutCancel(ctx)
	purging.Add(1)
	helpers.GoSafeNamed("tables.purge", func() {
		defer purging.Add(-1)
		purgeClaimed(later, model.Claim{ID: id, By: actor.UserID})
	})
	return nil
}

// purgeClaimed deletes a claimed table for good, recorded as removed by who
// claimed it (the server for its retention). A failure is logged: the table
// stays claimed, and the trash's sweep finishes it.
// forgetTablesForms takes the tables' own forms (no project) with them (V-121
// F1). The files those forms wear as cover or logo are archived, as a table's
// own files are, so the attachments purge takes the bytes; then the forms go
// for good in the same transaction, with their answers and public links. A
// file a visitor was still sending goes on the uploads sweep's next pass.
func forgetTablesForms(ctx context.Context, ids []uuid.UUID) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		looks, err := formModel.LooksOfTables(ctx, ids)
		if err != nil {
			return err
		}
		if _, err := attachmentModels.ArchiveFiles(ctx, looks, time.Now()); err != nil {
			return err
		}
		return formModel.ForgetOfTables(ctx, ids)
	})
}

func purgeClaimed(ctx context.Context, c model.Claim) {
	actor, reason := resourceEventBusiness.Actor{System: true}, purgedRetention
	if c.By != uuid.Nil {
		actor, reason = resourceEventBusiness.Actor{UserUUID: c.By.String()}, purgedForGood
	}
	if err := purgeTables(ctx, []uuid.UUID{c.ID}, actor, reason); errors.Is(err, errPagesLeft) {
		helpers.LogInfoWithContext(ctx, "tables.purge: table %s (%s): %v", c.ID, reason, err)
	} else if err != nil {
		helpers.LogErrorWithContext(ctx, "tables.purge: table %s (%s): %v; the trash's sweep finishes it", c.ID, reason, err)
	}
}

// aPass is what one pass of a delete for good takes (planPass).
type aPass struct {
	done  []uuid.UUID // the tables that go for good in this pass
	rows  []string    // their rows
	pages []uuid.UUID // the row pages it hands docs: all of done's, then the cut table's first
	cut   uuid.UUID   // the table cut short, which keeps its claim (uuid.Nil: none)
}

// planPass shares one pass's budget of row pages (docs take one trash
// operation's worth at a time) among the claimed tables in turn, fewest pages
// first, claim order between equals. Each table whose pages all fit goes for
// good in this pass. The first that doesn't takes what is left of the budget
// and keeps its claim, as do the tables after it, for the passes after
// (errPagesLeft). So one big table holds back no other (V-121 S1).
func planPass(ids []uuid.UUID, held []model.Holding, budget int) aPass {
	pagesOf := make(map[uuid.UUID][]uuid.UUID, len(ids))
	rowsOf := make(map[uuid.UUID][]string, len(ids))
	for _, h := range held {
		rowsOf[h.Table] = append(rowsOf[h.Table], h.Row.String())
		if h.Page != uuid.Nil {
			pagesOf[h.Table] = append(pagesOf[h.Table], h.Page)
		}
	}
	order := slices.Clone(ids)
	slices.SortStableFunc(order, func(a, b uuid.UUID) int { return len(pagesOf[a]) - len(pagesOf[b]) })
	var p aPass
	for _, id := range order {
		pages := pagesOf[id]
		if len(pages) > budget {
			p.cut = id
			p.pages = append(p.pages, pages[:budget]...)
			break
		}
		budget -= len(pages)
		p.done = append(p.done, id)
		p.rows = append(p.rows, rowsOf[id]...)
		p.pages = append(p.pages, pages...)
	}
	return p
}

// purgeTables deletes claimed tables for good: outside first, each through
// its owner, then the tables' own store, then one table.purged each. A pass
// takes the tables whose row pages fit its budget (planPass).
func purgeTables(ctx context.Context, ids []uuid.UUID, by resourceEventBusiness.Actor, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	held, err := model.HoldingsOfTables(ctx, ids)
	if err != nil {
		return err
	}
	pass := planPass(ids, held, hostedPagesPerPass)
	// The pages' guests are put out first, as the tables' are below: docs'
	// purge leaves a page's guest links (V-121 S2).
	if len(pass.pages) > 0 {
		if err := forgetGuestLinks(ctx, accessBusiness.KindDoc, helpers.UUIDStrings(pass.pages)); err != nil {
			return err
		}
	}
	if _, err := purgeHostedDocs(ctx, pass.pages, by, reason); err != nil {
		return err
	}
	if pass.cut != uuid.Nil {
		// Its rows forget the pages docs took, so the next pass finds only
		// the pages left (HoldingsOfTables).
		if err := model.ForgetRowPages(ctx, []uuid.UUID{pass.cut}, pass.pages); err != nil {
			return err
		}
	}
	if len(pass.done) == 0 {
		return errPagesLeft
	}
	rows, tables := pass.rows, helpers.UUIDStrings(pass.done)
	if err := forgetThreads(ctx, tableAccess.KindTableRow, rows); err != nil {
		return err
	}
	if err := forgetFollows(ctx, tableAccess.KindTableRow, rows); err != nil {
		return err
	}
	if _, err := archivePlaceFiles(ctx, postgresStruct.ATTACHMENT_SRC_TABLE, tables, time.Now()); err != nil {
		return err
	}
	if err := forgetGuestLinks(ctx, tableAccess.KindTable, tables); err != nil {
		return err
	}
	if err := forgetPlaces(ctx, tableAccess.KindTable, tables); err != nil {
		return err
	}
	if err := forgetTablesForms(ctx, pass.done); err != nil {
		return err
	}
	gone, err := model.DeleteTablesForGood(ctx, pass.done)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, id := range gone {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.TablePurged, ResourceKind: tableAccess.KindTable, ResourceUUID: id.String(),
			Actor: by, At: now, Data: map[string]string{"reason": reason},
		})
	}
	// The tables that linked to them lost those links with their rows.
	tellLinkedLater(ctx, gone...)
	if pass.cut != uuid.Nil {
		return errPagesLeft
	}
	return nil
}

// trashPurge is the trash's sweep (business/RowLedger SweepTrashPurge). It
// finishes a delete for good a stopped server left part way, in the name of
// whoever started it; past the policy's days, it deletes for good the tables
// in the trash, through the same delete, then the rows and fields in tables'
// own trash. The policy is 0, keep, until the owner decides (PROGRAM.md §13
// O3), so today it only finishes deletes for good.
func trashPurge(ctx context.Context, days int, now time.Time) error {
	if c, found, err := claimStalePurge(ctx, now.Add(-purgeStale), now); err != nil {
		return err
	} else if found {
		purgeClaimed(ctx, c)
	}
	if days <= 0 {
		return nil
	}
	cutoff := now.AddDate(0, 0, -days)
	ids, err := model.ClaimTablesPastRetention(ctx, cutoff, now, trashPurgeBatch)
	if err != nil {
		return err
	}
	// Tables with pages left stay claimed: a later pass finishes them as stale.
	if err := purgeTables(ctx, ids, resourceEventBusiness.Actor{System: true}, purgedRetention); err != nil && !errors.Is(err, errPagesLeft) {
		return err
	}
	rows, fields, err := model.PurgeTrash(ctx, cutoff)
	if err == nil && rows+fields > 0 {
		helpers.LogInfoWithContext(ctx, "tables.trash.purge: removed %d rows and %d fields", rows, fields)
	}
	return err
}
