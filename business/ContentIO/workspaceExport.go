package business

// The workspace export: an admin's zip of every doc, as Markdown, in folders
// that follow the page tree (a page's subpages sit in a folder with its name),
// which an import of the zip reads back as the same tree.
//
// WHAT GOES IN. Every live doc the admin can open, and the private docs whose
// owners let them in (doc_export_consents, migration 343: SetExportConsent).
// Every other private doc is left out, and the job says how many; docs in the
// trash or the archive are not live, so they aren't in it. Starting an export
// and every consent are recorded in the admin audit log by the routes that
// take them (controllers/ContentIO).
//
// It runs as a job of the pipeline's queue (kind "export"), so it survives a
// restart and its owner follows it like an import; the zip is kept in storage
// for a day, then the cleanup removes it.

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	docExportModels "github.com/akashc777/OneCamp/models/postgres/DocExport"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// workspaceFormat is a workspace export's format on its job row.
const workspaceFormat = "workspace-md"

// exportPage is how many docs a workspace export reads at a time.
const exportPage = 200

// downloadFor is how long a finished export's download address stays good.
const downloadFor = 15 * time.Minute

// recentExports is how many of an admin's exports the card lists.
const recentExports = 5

// ErrNotAdmin is a workspace export asked for by someone who isn't an admin.
var ErrNotAdmin = errors.New("content io: only an admin can export the workspace")

// Seams for tests: the docs there are, a page at a time; their rows; the
// consents; and whether someone is an admin.
var (
	docUUIDPage = docDomain.GetDocUUIDPage
	docRowsOf   = docBusiness.DocRows
	consentsFor = func(ctx context.Context, ids []string) (map[string]string, error) {
		var parsed []uuid.UUID
		for _, id := range ids {
			if u, err := uuid.Parse(id); err == nil {
				parsed = append(parsed, u)
			}
		}
		given, err := docExportModels.Given(ctx, parsed)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(given))
		for doc, owner := range given {
			out[doc.String()] = owner.String()
		}
		return out, nil
	}
	isAdmin = func(ctx context.Context, userUUID string) bool {
		id, err := uuid.Parse(userUUID)
		if err != nil {
			return false
		}
		u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		return err == nil && u != nil && u.IsAdmin
	}
)

// ExportWorkspace starts an admin's export of the workspace's docs. While an
// export they started is still waiting or running, it answers that one with
// ErrConflict instead of starting another.
func ExportWorkspace(ctx context.Context, who accessBusiness.Subject) (Job, error) {
	userID, err := uuid.Parse(who.UserUUID)
	if err != nil || !isAdmin(ctx, who.UserUUID) {
		return Job{}, ErrNotAdmin
	}
	recent, err := jobs.Recent(ctx, userID, "export", workspaceFormat, 1)
	if err != nil {
		return Job{}, err
	}
	if len(recent) == 1 && (recent[0].Status == contentIOModels.StatusQueued || recent[0].Status == contentIOModels.StatusRunning) {
		return toJob(recent[0]), ErrConflict
	}
	id := uuid.New()
	row := &contentIOModels.Job{ID: id, Kind: "export", Format: workspaceFormat, UserUUID: userID, Status: contentIOModels.StatusQueued}
	if err := jobs.Create(ctx, row); err != nil {
		return Job{}, err
	}
	return thePipeline.announce(ctx, id)
}

// WorkspaceExport is one of who's workspace exports, with an address to
// download it from once it is done ("" before, and once the zip is gone).
func WorkspaceExport(ctx context.Context, who accessBusiness.Subject, id string) (Job, string, error) {
	row, err := own(ctx, who, id)
	if err != nil {
		return Job{}, "", err
	}
	if row.Kind != "export" || row.Format != workspaceFormat {
		return Job{}, "", ErrNotFound
	}
	job := toJob(row)
	if row.Status != contentIOModels.StatusDone || row.OutputKey == nil {
		return job, "", nil
	}
	url, err := store.PresignGet(ctx, *row.OutputKey, downloadFor, "workspace-docs.zip")
	if err != nil {
		return job, "", err
	}
	return job, url, nil
}

// RecentWorkspaceExports are who's last few workspace exports, newest first.
func RecentWorkspaceExports(ctx context.Context, who accessBusiness.Subject) ([]Job, error) {
	userID, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := jobs.Recent(ctx, userID, "export", workspaceFormat, recentExports)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(rows))
	for _, r := range rows {
		out = append(out, toJob(r))
	}
	return out, nil
}

// exportDoc is one doc going into a workspace export.
type exportDoc struct {
	uuid, title, parent string
}

// runWorkspaceExport does one workspace export, start to end. Like run, it
// never returns an error: the job records how it ended.
func (p *pipeline) runWorkspaceExport(ctx context.Context, row *contentIOModels.Job) {
	var res jobResult
	var outKey *string
	finish := func(msg string) {
		status := contentIOModels.StatusDone
		var errText *string
		if msg != "" {
			status = contentIOModels.StatusFailed
			errText = &msg
		}
		result, _ := json.Marshal(res)
		if err := jobs.Finish(ctx, row.ID, status, result, outKey, errText); err != nil {
			helpers.LogErrorWithContext(ctx, "content io: workspace export %s could not be finished: %v", row.ID, err)
		}
		_, _ = p.announce(ctx, row.ID)
	}
	who, _, err := subjectFor(ctx, row.UserUUID.String())
	if err != nil || !isAdmin(ctx, row.UserUUID.String()) {
		finish("Only an admin can export the workspace, and whoever started this export no longer is one.")
		return
	}
	progress := newProgress(ctx, p, row)
	docs, leftOut, err := exportableDocs(ctx, who, func(done int) { progress.set(min(30, 1+done/exportPage)) })
	if err != nil {
		helpers.LogErrorWithContext(ctx, "content io: workspace export %s could not list the docs: %v", row.ID, err)
		finish("The docs could not be read just now. Start the export again in a few minutes.")
		return
	}
	if leftOut > 0 {
		res.Skipped = append(res.Skipped, Skipped{
			What: fmt.Sprintf("%d private %s", leftOut, plural(leftOut, "doc", "docs")),
			Why:  "their owners haven't let them into workspace exports",
		})
	}
	f, err := os.CreateTemp("", "workspace-export-*.zip")
	if err != nil {
		finish("The export could not be written on the server. Try again shortly.")
		return
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	written, skipped, err := writeWorkspaceZip(ctx, f, who, docs, func(done int) { progress.set(scaled(done, len(docs), 30, 95)) })
	if err != nil {
		helpers.LogErrorWithContext(ctx, "content io: workspace export %s could not be written: %v", row.ID, err)
		finish("The export could not be written on the server. Try again shortly.")
		return
	}
	res.Skipped = append(res.Skipped, skipped...)
	res.Exported = written
	size, err := f.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		finish("The export could not be written on the server. Try again shortly.")
		return
	}
	key := "content-io/" + row.ID.String() + "/workspace-docs.zip"
	if _, err := store.Put(ctx, key, f, size, "application/zip"); err != nil {
		helpers.LogErrorWithContext(ctx, "content io: workspace export %s could not be stored: %v", row.ID, err)
		finish("The export could not be stored. Try again shortly.")
		return
	}
	outKey = &key
	finish("")
}

// exportableDocs is every live doc who can open, and every private doc whose
// owner let it in, in the graph's order, and how many private docs were left
// out.
func exportableDocs(ctx context.Context, who accessBusiness.Subject, report func(done int)) ([]exportDoc, int, error) {
	var out []exportDoc
	leftOut := 0
	for offset := 0; ; offset += exportPage {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		ids, err := docUUIDPage(ctx, offset, exportPage)
		if err != nil {
			return nil, 0, err
		}
		if len(ids) == 0 {
			return out, leftOut, nil
		}
		rows, err := docRowsOf(ctx, ids, who.UserUID)
		if err != nil {
			return nil, 0, err
		}
		res := make([]accessBusiness.Resource, 0, len(rows))
		for _, d := range rows {
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.Uuid})
		}
		decisions, err := accessBusiness.ResolveAll(ctx, who, res)
		if err != nil {
			return nil, 0, err
		}
		consents, err := consentsFor(ctx, ids)
		if err != nil {
			return nil, 0, err
		}
		for _, d := range rows {
			// A table row's body, or a row's page (a hosted doc, D-18), goes
			// with its table, never as a doc of the workspace's.
			if d == nil || d.Uuid == "" || helpers.IsSoftDeleted(d.DeletedAt) || helpers.IsSoftDeleted(d.ArchivedAt) || d.SpaceKind == "table" || docDomain.IsHosted(d) {
				continue
			}
			decision := decisions[accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.Uuid}]
			switch {
			case !decision.Deleted && decision.Level >= accessBusiness.LevelView:
			case consents[d.Uuid] != "" && consents[d.Uuid] == ownerUUID(d):
			default:
				leftOut++
				continue
			}
			out = append(out, exportDoc{uuid: d.Uuid, title: d.Title, parent: parentUUID(d)})
		}
		report(offset + len(ids))
	}
}

// ownerUUID is who owns a doc (docDomain.OwnerOf: after a hand-over, its new
// owner).
func ownerUUID(d *dgraphStruct.DgraphDoc) string {
	if o := docDomain.OwnerOf(d); o != nil {
		return o.Uuid
	}
	return ""
}

func parentUUID(d *dgraphStruct.DgraphDoc) string {
	if d.Parent != nil {
		return d.Parent.Uuid
	}
	return ""
}

// writeWorkspaceZip writes the docs as Markdown files, each in the folder of
// the page it is under when that page is in the export too, and says how many
// it wrote and which it couldn't read. An error is the zip itself failing
// (the disk, or the export stopping): what it wrote is no export.
func writeWorkspaceZip(ctx context.Context, w io.Writer, who accessBusiness.Subject, docs []exportDoc, report func(done int)) (written int, skipped []Skipped, err error) {
	paths := exportPaths(docs)
	zw := zip.NewWriter(w)
	appURL := authService.FrontendBaseURL()
	for i, d := range docs {
		if err := ctx.Err(); err != nil {
			return written, skipped, err
		}
		title, stored, err := loadDoc(ctx, who, d.uuid)
		var body *contentcodec.Node
		if err == nil {
			body, err = contentcodec.FromHTML(stored, contentcodec.HTMLOptions{})
		}
		if err != nil {
			helpers.LogWarnWithContext(ctx, "content io: workspace export left out doc %s: %v", d.uuid, err)
			skipped = append(skipped, Skipped{What: exportTitle(d.title), Why: "it could not be read"})
			report(i + 1)
			continue
		}
		fw, err := zw.Create(paths[d.uuid] + ".md")
		if err == nil {
			_, err = io.WriteString(fw, docMarkdown(title, body, appURL))
		}
		if err != nil {
			return written, skipped, err
		}
		written++
		report(i + 1)
	}
	return written, skipped, zw.Close()
}

// exportPaths is each doc's path in the zip, without its extension: its
// parent's path and its own name, or its name alone when its parent isn't in
// the export. Two siblings of one name are told apart by a number. Pure.
func exportPaths(docs []exportDoc) map[string]string {
	byID := make(map[string]exportDoc, len(docs))
	for _, d := range docs {
		byID[d.uuid] = d
	}
	paths := make(map[string]string, len(docs))
	taken := map[string]bool{}
	var place func(d exportDoc, seen map[string]bool) string
	place = func(d exportDoc, seen map[string]bool) string {
		if p, ok := paths[d.uuid]; ok {
			return p
		}
		dir := ""
		if parent, ok := byID[d.parent]; ok && d.parent != d.uuid && !seen[d.parent] {
			seen[d.uuid] = true
			dir = place(parent, seen)
		}
		name := uploadsafe.SanitiseFileName(exportTitle(d.title), 80)
		p := path.Join(dir, name)
		for n := 2; taken[strings.ToLower(p)]; n++ {
			p = path.Join(dir, fmt.Sprintf("%s (%d)", name, n))
		}
		taken[strings.ToLower(p)] = true
		paths[d.uuid] = p
		return p
	}
	for _, d := range docs {
		place(d, map[string]bool{})
	}
	return paths
}

// exportTitle is a doc's title, or "Untitled" for one with none (D-6).
func exportTitle(t string) string {
	if t = strings.TrimSpace(t); t != "" {
		return t
	}
	return "Untitled"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
