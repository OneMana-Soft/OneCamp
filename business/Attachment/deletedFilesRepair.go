package business

// The files of posts, messages and comments deleted before a delete took them
// off with the item (ArchiveFilesOf), taken off once per install.
//
// Until then, deleting one marked its files deleted in search only, and the
// purge, which takes only stamped rows, never took their bytes. This reads the items deleted in
// Postgres (every delete path and the Slack import's tombstones set
// deleted_at; nothing restores one), keeps those the graph holds deleted too
// (a delete that stopped half way leaves the item showing, and its files
// with it), and takes each one's files off by the rule a delete uses now:
// what an item still there holds stays. The rows it stamps enter the trash at
// the run's time, so the purge takes them after the normal retention, never
// at once. It goes a page at a time, keeping where it got to (Progress), and
// is idempotent: an item done before stamps nothing again. An item that
// fails on five runs in a row (with the runner's backoff, eight minutes and
// more) is passed and named in the log, so one bad item can't hold the rest
// back; any other failure stops the run, to go on from there later. The audit
// log records what it did, in counts.

import (
	"context"
	"fmt"
	"time"

	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	domain "github.com/akashc777/OneCamp/domain/Attachment"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

const (
	deletedFilesRepairKey  = "deleted_items_files_v1"
	deletedFilesRepairPage = 200
	// deletedFilesTries is how many runs in a row one item may fail on
	// before the repair passes it.
	deletedFilesTries = 5
)

// DeletedFilesReport is what the repair did: the posts, messages and comments
// deleted earlier whose files it took off, the files it took, and the items
// it passed after they failed deletedFilesTries runs in a row.
type DeletedFilesReport struct {
	Posts    int `json:"posts"`
	Chats    int `json:"chats"`
	Comments int `json:"comments"`
	Files    int `json:"files"`
	Passed   int `json:"passed"`
}

func (r DeletedFilesReport) String() string {
	return fmt.Sprintf("%d posts, %d messages and %d comments deleted earlier: %d files taken off with them; %d items passed after failing %d runs",
		r.Posts, r.Chats, r.Comments, r.Files, r.Passed, deletedFilesTries)
}

// DeletedFilesRepair runs RepairDeletedFiles once per install.
var DeletedFilesRepair = repairBusiness.Once[DeletedFilesReport]{
	Key: deletedFilesRepairKey, Name: "attachment.deleted-files-repair", Tries: 12, Repair: RepairDeletedFiles,
}

// deletedFilesProgress is where the repair got to: the last item of each kind
// it is past, the item a run stopped at and on how many runs in a row, and
// what it has done so far.
type deletedFilesProgress struct {
	After  map[string]uuid.UUID `json:"after"`
	Stuck  uuid.UUID            `json:"stuck"`
	Tries  int                  `json:"tries"`
	Report DeletedFilesReport   `json:"report"`
}

// The kinds of item the repair reads, in order, and which count each adds to.
var deletedKinds = []struct {
	name   string
	holder domain.Holder
	count  func(*DeletedFilesReport) *int
}{
	{"posts", PostFiles, func(r *DeletedFilesReport) *int { return &r.Posts }},
	{"chats", ChatFiles, func(r *DeletedFilesReport) *int { return &r.Chats }},
	{"comments", CommentFiles, func(r *DeletedFilesReport) *int { return &r.Comments }},
}

// Where the repair reads and writes: seams.
var (
	deletedItems   = domain.DeletedItems
	deletedInGraph = domain.DeletedInGraph
	archiveFilesOf = ArchiveFilesOf
	loadProgress   = repairBusiness.Progress[deletedFilesProgress]
	saveProgress   = func(p deletedFilesProgress) error { return repairBusiness.SaveProgress(deletedFilesRepairKey, p) }
	recordRepair   = recordDeletedFilesRepair
	repairNow      = time.Now
)

// RepairDeletedFiles takes off the files of the items deleted before a
// delete took them, from where an earlier run got to (see the top of this
// file). It answers what it has done in all its runs.
func RepairDeletedFiles(ctx context.Context) (DeletedFilesReport, error) {
	p, err := loadProgress(deletedFilesRepairKey)
	if err != nil {
		return DeletedFilesReport{}, err
	}
	if p.After == nil {
		p.After = map[string]uuid.UUID{}
	}
	at := repairNow()
	for _, kind := range deletedKinds {
		for {
			ids, err := deletedItems(ctx, kind.holder, p.After[kind.name], deletedFilesRepairPage)
			if err != nil {
				return p.Report, err
			}
			if len(ids) == 0 {
				break
			}
			gone, err := deletedInGraph(ctx, kind.holder, ids)
			if err != nil {
				return p.Report, err
			}
			for _, id := range ids {
				if gone[id] {
					taken, err := archiveFilesOf(ctx, kind.holder, id, at)
					if err != nil && !p.passes(ctx, kind.name, id, err) {
						return p.Report, saveAfter(p, fmt.Errorf("%s %s: %w", kind.name, id, err))
					}
					if err == nil {
						*kind.count(&p.Report)++
						p.Report.Files += taken
					}
				}
				p.After[kind.name] = id
			}
			if err := saveProgress(p); err != nil {
				return p.Report, err
			}
			if len(ids) < deletedFilesRepairPage {
				break
			}
		}
	}
	if p.Report.Posts+p.Report.Chats+p.Report.Comments+p.Report.Passed > 0 {
		recordRepair(ctx, p.Report)
	}
	return p.Report, nil
}

// passes says whether the repair goes past item id of kind, which failed with
// err: when it has failed on deletedFilesTries runs in a row, counting this
// one. Otherwise it is the item the run stops at.
func (p *deletedFilesProgress) passes(ctx context.Context, kind string, id uuid.UUID, err error) bool {
	if p.Stuck == id {
		p.Tries++
	} else {
		p.Stuck, p.Tries = id, 1
	}
	if p.Tries < deletedFilesTries {
		return false
	}
	helpers.LogErrorWithContext(ctx, "attachment.deleted-files-repair: passed %s %s after %d runs failed on it: %+v", kind, id, p.Tries, err)
	p.Report.Passed++
	p.Stuck, p.Tries = uuid.Nil, 0
	return true
}

// saveAfter keeps where the repair stopped, and answers the error that
// stopped it with any failure to keep it.
func saveAfter(p deletedFilesProgress, stopped error) error {
	if err := saveProgress(p); err != nil {
		return fmt.Errorf("%w (and keeping where it stopped: %v)", stopped, err)
	}
	return stopped
}

// recordDeletedFilesRepair tells the admins, in the audit log, what the
// repair did: counts alone.
func recordDeletedFilesRepair(ctx context.Context, r DeletedFilesReport) {
	summary := fmt.Sprintf("Took the files of %d posts, %d messages and %d comments deleted earlier off with them: %d files went to the trash",
		r.Posts, r.Chats, r.Comments, r.Files)
	if r.Passed > 0 {
		summary += fmt.Sprintf("; %d items were passed after failing %d times (the server log names them)", r.Passed, deletedFilesTries)
	}
	err := adminAuditBusiness.RecordForPrincipal(ctx, nil, "", adminAuditBusiness.ActorSystem,
		"files.deleted_messages_repaired", adminAuditBusiness.CategorySecurity, summary,
		map[string]interface{}{"posts": r.Posts, "chats": r.Chats, "comments": r.Comments, "files": r.Files, "passed": r.Passed})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "attachment.deleted-files-repair: the audit record: %+v", err)
	}
}
