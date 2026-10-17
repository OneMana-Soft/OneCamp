package business

// The Content IO Pipeline: importers and exporters as adapters, jobs with
// progress, an asset uploader so no base64 ever lands in a body, and docs
// created through the tree and the Live Doc Writer's seed (docs programme,
// PROGRAM.md §4 #17, built in A7 beside the Content Codec).
//
// An importer reads a file and hands the pipeline pages; the pipeline makes
// the docs. An exporter writes one resource in one format. Each registers
// itself from its package's init():
//
//	func init() { contentIOBusiness.Pipeline.RegisterImporter(markdownImporter{}) }
//
// Imports run as jobs: the file is kept in object storage, a worker (the
// "content-io.run" sweep) makes the docs, and the person who started it
// follows the job over MQTT (MESSAGE_CONTENT_IO_JOB) or GET
// /content-io/jobs/{id}. Slack, calendar and transcript imports are meant to
// become adapters here too, with the matching exports.

import (
	"context"
	"errors"
	"io"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
)

// Importer turns a file into new resources.
type Importer interface {
	// Format names it: "md", "txt", "html", "docx", "zip".
	Format() string
	// Accepts says whether it reads a file with this name and media type.
	Accepts(filename, mediaType string) bool
	// Import reads in and hands what it finds to out.
	Import(ctx context.Context, in Input, out Output) error
}

// Input is the file an import reads.
type Input struct {
	Filename  string
	MediaType string
	Size      int64
	// File is the whole file: a zip or a Word file is read out of order.
	File io.ReaderAt
}

// Reader reads the file from its start.
func (in Input) Reader() io.Reader { return io.NewSectionReader(in.File, 0, in.Size) }

// Output takes what an importer finds.
type Output interface {
	// Page adds a page. Pages become docs once the importer is done, parents
	// first, so a link from one page to another can be pointed at the doc it
	// became.
	Page(p Page) error
	// Progress says how far reading has got.
	Progress(done, total int)
	// Skip records something left out, and why.
	Skip(what, why string)
}

// Page is one doc an import makes.
type Page struct {
	// Key names it within the import: its path in a zip, or the file's name.
	Key string
	// ParentKey is another page's Key; "" puts it under the import's parent.
	ParentKey string
	Title     string
	// Icon is the emoji the page shows beside its title, when the tool it
	// comes from gave it one.
	Icon string
	Body *contentcodec.Node
	// Assets opens the files the body refers to by a relative path, such as
	// an image beside it in a zip; nil when there are none.
	Assets AssetOpener
}

// AssetOpener opens a file an imported body refers to.
type AssetOpener interface {
	// Open opens the file at path (relative to the page), with its size.
	Open(path string) (io.ReadCloser, int64, error)
}

// Exporter writes one kind of resource in one format.
type Exporter interface {
	// Kind is the resource kind it writes: "doc".
	Kind() string
	// Format names it: "md", "html", "json".
	Format() string
	// Export writes res, which who may read, to the target.
	Export(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, to ExportTarget) error
}

// ExportTarget is where an export goes: told the file's name and type before
// anything is written.
type ExportTarget interface {
	Start(filename, mediaType string) io.Writer
}

// JobStatus is where a job stands.
type JobStatus string

const (
	// JobUploading waits for its file (a large one goes straight to storage).
	JobUploading JobStatus = "uploading"
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
)

// Job is one import or export, with its progress, as its owner sees it.
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"` // "import" or "export"
	Format   string    `json:"format"`
	UserUUID string    `json:"user_uuid"`
	Status   JobStatus `json:"status"`
	Progress int       `json:"progress"` // 0–100
	Filename string    `json:"filename,omitempty"`
	// ParentUUID is where imported pages go; "" is the top.
	ParentUUID string `json:"parent_uuid,omitempty"`
	Error      string `json:"error,omitempty"`
	// Created is what an import made, in the order it made them.
	Created []Created `json:"created,omitempty"`
	// Skipped is what it left out, and why.
	Skipped []Skipped `json:"skipped,omitempty"`
	// Exported is how many docs a workspace export wrote.
	Exported   int        `json:"exported,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// KeptUntil is when the file an export made is removed; absent once it
	// is, and for a job that made none.
	KeptUntil *time.Time `json:"kept_until,omitempty"`
}

// Created is one resource an import made.
type Created struct {
	Kind       string `json:"kind"`
	UUID       string `json:"uuid"`
	Title      string `json:"title"`
	ParentUUID string `json:"parent_uuid,omitempty"`
}

// Skipped is one thing an import left out.
type Skipped struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// ImportRequest is a file to import and where its pages go.
type ImportRequest struct {
	Filename   string
	MediaType  string
	ParentUUID string
	// Size is the file's size when known, -1 when not.
	Size int64
	// Body is the file; empty for PrepareUpload, where it comes later.
	Body io.Reader
}

// Upload is where to put a large file: the browser sends it straight to
// object storage, then asks for the import to start.
type Upload struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresIn int64             `json:"expires_in"`
}

// Capabilities is what the pipeline can do for someone, for the web app's
// menus.
type Capabilities struct {
	// Export is whether they may export docs (the docs.export.members policy;
	// admins always may).
	Export bool `json:"export"`
	// ExportFormats are the formats the server writes; the browser also makes
	// PDF and Word files itself.
	ExportFormats []string `json:"export_formats"`
	// ImportExtensions are the files an import reads.
	ImportExtensions []string `json:"import_extensions"`
	// MaxImportBytes is the largest file an import takes.
	MaxImportBytes int64 `json:"max_import_bytes"`
	// MaxDirectBytes is the largest file sent with the request itself; a
	// larger one goes to storage first (PrepareUpload).
	MaxDirectBytes int64 `json:"max_direct_bytes"`
}

// Service is the Content IO Pipeline.
type Service interface {
	RegisterImporter(i Importer)
	RegisterExporter(e Exporter)
	// Import starts an import of a file sent with the request, and returns its
	// job at once.
	Import(ctx context.Context, who accessBusiness.Subject, req ImportRequest) (Job, error)
	// PrepareUpload starts an import of a file too large to send with the
	// request: the job waits for it at the Upload's address.
	PrepareUpload(ctx context.Context, who accessBusiness.Subject, req ImportRequest) (Job, Upload, error)
	// StartUploaded queues an import whose file has arrived.
	StartUploaded(ctx context.Context, who accessBusiness.Subject, id string) (Job, error)
	// Export writes res in a format, checked against who's access.
	Export(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format string, to ExportTarget) error
	// RecordExport records an export the browser made (Markdown, HTML, PDF,
	// Word), after the same access check.
	RecordExport(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format string) error
	// Job is a job's state, for the person who started it.
	Job(ctx context.Context, who accessBusiness.Subject, id string) (Job, error)
	// Capabilities is what the pipeline can do for who.
	Capabilities(ctx context.Context, who accessBusiness.Subject, isAdmin bool) Capabilities
}

var (
	// ErrUnavailable is an operation the pipeline can't do here: no storage,
	// no database.
	ErrUnavailable = errors.New("content io: not available here")
	// ErrUnsupported is a file no importer reads, or a format no exporter
	// writes.
	ErrUnsupported = errors.New("content io: not a format this reads or writes")
	// ErrTooLarge is a file over the import limit.
	ErrTooLarge = errors.New("content io: the file is too large")
	// ErrForbidden is an operation who may not do: an import under a page they
	// can't edit, an export of one they can't read.
	ErrForbidden = errors.New("content io: not allowed")
	// ErrNotFound is a job or a resource that isn't there, or isn't theirs.
	ErrNotFound = errors.New("content io: not found")
	// ErrConflict is a job that has moved on: an upload started twice.
	ErrConflict = errors.New("content io: the job has moved on")
)

// Pipeline is the pipeline everything asks.
var Pipeline Service = newPipeline()
