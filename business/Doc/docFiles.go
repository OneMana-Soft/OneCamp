package businness

// The doc tree's files (docs F1, Docs & Files): a file is a doc node whose
// versions are attachments rows at its place, each named in doc_file_versions
// (models/postgres/DocFile) with its name, size and type. The node's file is
// its newest version. What a row shows of a file, what a purge leaves of one,
// and what a duplicate copies are here.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	"github.com/google/uuid"
)

// FileFacts is what a row shows of a file (the F1a/F1b contract, 2): its
// newest version's name, size and type, how many versions it has, and the
// attachment its download asks for (GET /doc/getFile/{file}/{attachment_id}).
type FileFacts struct {
	Name         string `json:"name"`
	SizeBytes    int64  `json:"size_bytes"`
	MIME         string `json:"mime"`
	Versions     int    `json:"versions"`
	AttachmentID string `json:"attachment_id"`
}

// currentFiles reads files' newest versions (a seam for tests).
var currentFiles = docFileModels.CurrentOf

// FileFactsOf is the file facts of the files among docs, by uuid, in one read
// (= ANY) for them all: a tree's level, a list's page. Docs that aren't files
// are left out, and with none, nothing is read.
func FileFactsOf(ctx context.Context, docs []*dgraphStruct.DgraphDoc) (map[string]*FileFacts, error) {
	out := map[string]*FileFacts{}
	var ids []uuid.UUID
	for _, d := range docs {
		if d == nil || d.NodeKind != domain.NodeFile {
			continue
		}
		if id, err := uuid.Parse(d.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	current, err := currentFiles(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/FileFactsOf err: %+v", err)
		return nil, err
	}
	for id, c := range current {
		out[strings.ToLower(id.String())] = &FileFacts{Name: c.Name, SizeBytes: c.SizeBytes, MIME: c.MIME,
			Versions: c.Versions, AttachmentID: c.AttachmentID.String()}
	}
	return out, nil
}

// forgetFiles takes the versions of docs gone for good and archives their
// attachments rows, so the attachments purge takes the bytes (a seam for
// tests). A page or a folder has none, and is left as it was.
var forgetFiles = docFileModels.ForgetFiles

// forgetPurgedFiles is forgetFiles for docs purged at at, by uuid.
func forgetPurgedFiles(ctx context.Context, uuids []string, at time.Time) error {
	ids := make([]uuid.UUID, 0, len(uuids))
	for _, s := range uuids {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	_, err := forgetFiles(ctx, ids, at)
	return err
}

// copyFileVersions gives each of a duplicate's file copies its source's
// newest version, as by (a person's uuid), in one statement (a seam for
// tests).
var copyFileVersions = func(ctx context.Context, files []docFileModels.Copy, by string) error {
	byID, err := uuid.Parse(by)
	if err != nil {
		return err
	}
	n, err := docFileModels.CopyCurrent(ctx, files, byID)
	if err == nil && int(n) != len(files) {
		helpers.LogWarnWithContext(ctx, "business/copyFileVersions: copied %d of %d files' versions; a source had none", n, len(files))
	}
	return err
}

// dropUnseenFiles takes back the versions given to copies that were never
// made (a seam for tests).
var dropUnseenFiles = func(ctx context.Context, files []docFileModels.Copy) error {
	ids := make([]uuid.UUID, len(files))
	for i, f := range files {
		ids[i] = f.To
	}
	_, err := docFileModels.DropUnseen(ctx, ids)
	return err
}

// fileCopies is, of a duplicate's copies, the files: each from its source to
// its copy, with the attachment id its copy's version 1 takes. Pure.
func fileCopies(nodes []domain.NewDocNode, copies []Copy) []docFileModels.Copy {
	var out []docFileModels.Copy
	for i, n := range nodes {
		if n.Doc == nil || n.Doc.NodeKind != domain.NodeFile || i >= len(copies) {
			continue
		}
		from, err1 := uuid.Parse(copies[i].SourceUUID)
		to, err2 := uuid.Parse(n.Doc.Uuid)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, docFileModels.Copy{From: from, To: to, AttachmentID: uuid.New()})
	}
	return out
}

// The rules of a file's versions.
var (
	// ErrNotAFile is a file's action asked of a node that isn't one.
	ErrNotAFile = errors.New("doc: that isn't a file")
	// ErrNoteTooLong is a version's note past maxVersionNote characters.
	ErrNoteTooLong = errors.New("doc: a version's note is at most 500 characters")
	// ErrNotStored wraps an upload that couldn't be stored: refused by its
	// check or its scan, or storage that didn't answer (what it wraps says
	// which: helpers.WriteUploadFailure).
	ErrNotStored = errors.New("doc: the file wasn't stored")
)

const (
	// maxVersionNote bounds a version's note, as doc_file_versions does.
	maxVersionNote = 500
	// maxFileName bounds a file's name as it is shown and kept.
	maxFileName = 255
)

// Seams, so the rules are tested without object storage, Postgres or the
// graph.
var (
	storeUpload    = attachmentBusiness.StoreUpload
	removeStored   = attachmentBusiness.RemoveStored
	recordVersion  = docFileModels.RecordVersion
	listVersions   = docFileModels.Versions
	dropUnseenDocs = docFileModels.DropUnseen
	makeFileNode   = CreateDocInTree
	touchDoc       = domain.TouchDoc
	indexVersion   = IndexFiles
)

// fileStoreOptions is how a file's bytes are stored: checked and scanned, a
// download forced except for the safe images a preview shows inline.
var fileStoreOptions = attachmentBusiness.StoreOptions{Safe: uploadsafe.Options{AllowInlineImages: true}}

// fileName is an upload's name as a file's title and its version's name: no
// path, no control characters, at most maxFileName characters. Pure.
func fileName(raw string) string {
	return helpers.TruncateRunes(helpers.SanitizeUploadFileName(strings.TrimSpace(raw)), maxFileName)
}

// versionNote is a version's note as it is kept: trimmed, at most
// maxVersionNote characters (ErrNoteTooLong). Pure.
func versionNote(raw string) (string, error) {
	note := strings.TrimSpace(raw)
	if utf8.RuneCountInString(note) > maxVersionNote {
		return "", ErrNoteTooLong
	}
	return note, nil
}

// titleAfterVersion is a file's title once a version named next replaces the
// one named prev: next, unless someone renamed the file (its title isn't
// prev), and whether it changed. Pure.
func titleAfterVersion(title, prev, next string) (string, bool) {
	if title != prev || next == prev {
		return title, false
	}
	return next, true
}

// accountOf is a person's account id: the graph's user_uuid is users.id.
func accountOf(by *dgraphStruct.DgraphUser) (uuid.UUID, error) {
	if by == nil {
		return uuid.Nil, ErrNotFound
	}
	return uuid.Parse(by.Uuid)
}

// NewFile is a file to add to the tree, as POST /doc/files takes it: under a
// page or a folder (ParentUUID), or at the top of a space; right after a
// sibling, or last; and the upload, with its name and declared size (-1 for
// unknown).
type NewFile struct {
	ParentUUID string
	Space      string
	AfterUUID  string
	Name       string
	Body       io.Reader
	Size       int64
}

// AddFile adds a file to the tree for by, all or nothing: its bytes are
// stored (StoreUpload: checked, scanned), recorded as its version 1 under a
// node id chosen first, and then its node is made (CreateDocInTree) where in
// says, under parent (read and checked by the caller, nil for the top). A
// version that can't be recorded takes the bytes back; a node that can't be
// made (its parent went, or holds nothing) takes back the version and the
// bytes. The caller then gives it its parent's sharing and its search entry
// (IndexFiles), as it does for any new subpage.
func AddFile(ctx context.Context, by *dgraphStruct.DgraphUser, in NewFile, parent *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, *FileFacts, error) {
	account, err := accountOf(by)
	if err != nil {
		return nil, nil, err
	}
	// A file or a link holds nothing (D-30): refused before anything is
	// stored. The tree refuses it again as the node is placed.
	if parent != nil && !domain.HoldsChildren(parent) {
		return nil, nil, graph.ErrLeaf
	}
	name := fileName(in.Name)
	stored, err := storeUpload(ctx, in.Body, in.Size, name, account, fileStoreOptions)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNotStored, err)
	}
	id := uuid.New()
	v, err := recordVersion(ctx, docFileModels.NewVersion{DocUUID: id, AttachmentID: stored.ID, ObjKey: stored.ObjKey,
		Name: name, SizeBytes: stored.Size, MIME: stored.ContentType, CreatedBy: account})
	if err != nil {
		takeBackBytes(ctx, stored.ObjKey)
		return nil, nil, err
	}
	doc, err := makeFileNode(ctx, by, NewDoc{Title: name, ParentUUID: in.ParentUUID, Space: in.Space, AfterUUID: in.AfterUUID,
		NodeKind: domain.NodeFile, UUID: id.String()}, parent)
	if err != nil {
		if undo := helpers.CompensateOnFailure(ctx, "a file's version 1", func(ctx context.Context) error {
			_, err := dropUnseenDocs(ctx, []uuid.UUID{id})
			return err
		}); undo != nil {
			helpers.LogErrorWithContext(ctx, "business/AddFile: %v", undo)
		}
		takeBackBytes(ctx, stored.ObjKey)
		return nil, nil, err
	}
	return doc, &FileFacts{Name: v.Name, SizeBytes: v.SizeBytes, MIME: v.MIME, Versions: 1, AttachmentID: v.AttachmentID.String()}, nil
}

// takeBackBytes removes stored bytes whose rows couldn't be written.
func takeBackBytes(ctx context.Context, objKey string) {
	if undo := helpers.CompensateOnFailure(ctx, "an upload's bytes", func(ctx context.Context) error {
		return removeStored(ctx, objKey)
	}); undo != nil {
		helpers.LogErrorWithContext(ctx, "business/takeBackBytes: %v", undo)
	}
}

// FileVersion is a version as GET /doc/files/{uuid}/versions answers it.
type FileVersion struct {
	N            int       `json:"n"`
	AttachmentID string    `json:"attachment_id"`
	Name         string    `json:"name"`
	SizeBytes    int64     `json:"size_bytes"`
	MIME         string    `json:"mime"`
	Note         string    `json:"note,omitempty"`
	CreatedBy    *Person   `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// versionView is v as it is answered, by the person who added it.
func versionView(v docFileModels.Version, by *Person) FileVersion {
	if by == nil {
		by = &Person{UUID: v.CreatedBy.String()}
	}
	return FileVersion{N: v.N, AttachmentID: v.AttachmentID.String(), Name: v.Name, SizeBytes: v.SizeBytes, MIME: v.MIME,
		Note: v.Note, CreatedBy: by, CreatedAt: v.CreatedAt}
}

// AddFileVersion records a new version of file (read and checked by the
// caller: by may edit it) from an upload, with a note: its bytes stored, then
// recorded as the file's next version, or taken back. The version is the
// file's last change: when it was added and by whom, written together
// (domain.TouchDoc), so the file's row names its uploader beside its time.
// Its name becomes the file's title unless someone renamed the file
// (titleAfterVersion). The file's search entry moves to the new version
// (IndexFiles).
func AddFileVersion(ctx context.Context, by *dgraphStruct.DgraphUser, file *dgraphStruct.DgraphDoc, upload NewFile, rawNote string) (*FileVersion, error) {
	if file == nil || file.NodeKind != domain.NodeFile {
		return nil, ErrNotAFile
	}
	note, err := versionNote(rawNote)
	if err != nil {
		return nil, err
	}
	account, err := accountOf(by)
	if err != nil {
		return nil, err
	}
	fileID, err := uuid.Parse(file.Uuid)
	if err != nil {
		return nil, ErrNotFound
	}
	prev, err := currentFiles(ctx, []uuid.UUID{fileID})
	if err != nil {
		return nil, err
	}
	name := fileName(upload.Name)
	stored, err := storeUpload(ctx, upload.Body, upload.Size, name, account, fileStoreOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotStored, err)
	}
	v, err := recordVersion(ctx, docFileModels.NewVersion{DocUUID: fileID, AttachmentID: stored.ID, ObjKey: stored.ObjKey,
		Name: name, SizeBytes: stored.Size, MIME: stored.ContentType, Note: note, CreatedBy: account})
	if err != nil {
		takeBackBytes(ctx, stored.ObjKey)
		return nil, err
	}
	// Who and when together, at the version's own time, before the title
	// follows: no moment names one person beside another's time. The version
	// stands either way.
	if err := touchDoc(ctx, file.Uuid, by.Uuid, v.CreatedAt); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddFileVersion: %s's new version didn't name its uploader: %v", file.Uuid, err)
	}
	if title, changed := titleAfterVersion(file.Title, prev[fileID].Name, name); changed {
		// The version stands either way: a title that didn't follow is a
		// rename away.
		if err := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: file.Uuid, Title: &title}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/AddFileVersion: the title of %s didn't follow its new version: %v", file.Uuid, err)
		}
	}
	replaced := ""
	if c, ok := prev[fileID]; ok {
		replaced = c.AttachmentID.String()
	}
	indexVersion(ctx, []string{file.Uuid}, replaced)
	view := versionView(v, PersonOf(by))
	return &view, nil
}

// FileVersions is every version of a file, newest first, each naming who
// added it (one read of the people for them all). The caller has checked the
// file is one, and that the person may open it.
func FileVersions(ctx context.Context, fileUUID string) ([]FileVersion, error) {
	id, err := uuid.Parse(fileUUID)
	if err != nil {
		return nil, ErrNotFound
	}
	versions, err := listVersions(ctx, id)
	if err != nil {
		return nil, err
	}
	who := make([]string, 0, len(versions))
	for _, v := range versions {
		who = append(who, v.CreatedBy.String())
	}
	people, err := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, who)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Person, len(people))
	for _, p := range people {
		if p != nil {
			byID[strings.ToLower(p.Uuid)] = PersonOf(p)
		}
	}
	out := make([]FileVersion, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionView(v, byID[strings.ToLower(v.CreatedBy.String())]))
	}
	return out, nil
}

// fileSearchTimeout bounds one write of files' search entries.
const fileSearchTimeout = 2 * time.Minute

// IndexFiles writes, in the background, the attachments-index entry of each
// file's newest version (docs F1: a file is found by its name there, never by
// a doc entry), whole from the file as it is now: its title, and who may find
// it. A file made or copied a moment ago is written after its sharing, so its
// entry names the people it should. replaced is a version a newer one
// replaced, whose entry goes ("" for none).
func IndexFiles(ctx context.Context, fileUUIDs []string, replaced string) {
	if len(fileUUIDs) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("doc.file-search", func() {
		ctx, cancel := context.WithTimeout(ctx, fileSearchTimeout)
		defer cancel()
		if err := indexFiles(ctx, fileUUIDs, replaced); err != nil {
			helpers.LogErrorWithContext(ctx, "business/IndexFiles %v: %v", fileUUIDs, err)
		}
	})
}

// indexFiles is IndexFiles' write: one read of the files, one of their newest
// versions, one bulk write of their entries.
func indexFiles(ctx context.Context, fileUUIDs []string, replaced string) error {
	docs, err := domain.GetDocSearchEntries(ctx, fileUUIDs)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(docs))
	for _, d := range docs {
		if id, err := uuid.Parse(d.Uuid); err == nil && d.NodeKind == domain.NodeFile {
			ids = append(ids, id)
		}
	}
	current, err := currentFiles(ctx, ids)
	if err != nil {
		return err
	}
	entries := make([]*openSearchStruct.OpenSearchAttachment, 0, len(current))
	for _, d := range docs {
		id, err := uuid.Parse(d.Uuid)
		c, ok := current[id]
		if err != nil || !ok {
			continue
		}
		entries = append(entries, fileSearchEntry(d, c))
	}
	if err := attachmentDomain.IndexAttachmentEntries(ctx, entries); err != nil {
		return err
	}
	if replaced != "" {
		return attachmentDomain.ForgetAttachmentEntry(ctx, replaced)
	}
	return nil
}

// fileSearchEntry is a file's entry in the attachments index: its newest
// version, carrying the file's title, privacy and sharing as a doc's files'
// entries do (domain/Doc DocSearchEntryOf), and deleted with it. Pure.
func fileSearchEntry(d *dgraphStruct.DgraphDoc, c docFileModels.Current) *openSearchStruct.OpenSearchAttachment {
	e := domain.DocSearchEntryOf(d)
	private := e.DocPrivate
	return &openSearchStruct.OpenSearchAttachment{
		Uuid:                           c.AttachmentID.String(),
		AttachmentByUserUuid:           c.CreatedBy.String(),
		AttachmentFileName:             c.Name,
		AttachmentObjKey:               c.ObjKey,
		AttachmentDocUuid:              d.Uuid,
		AttachmentDocTitle:             e.DocTitle,
		AttachmentDocPrivate:           &private,
		AttachmentDocReadingUsers:      e.DocReadingUsers,
		AttachmentDocEditingUsers:      e.DocEditingUsers,
		AttachmentDocCommentingUsers:   e.DocCommentingUsers,
		AttachmentDocCreatedByUserUuid: e.DocCreatedByUserUuid,
		AttachmentCreatedAt:            c.CreatedAt.Unix(),
		AttachmentDeletedAt:            e.DocDeletedAt,
	}
}
