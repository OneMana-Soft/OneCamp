package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	"github.com/google/uuid"
)

// defaultMaxImportBytes is the largest file an import takes unless
// DOC_IMPORT_MAX_BYTES says otherwise: room for a wiki's export with its
// images, without letting one upload fill a small server's disk.
const defaultMaxImportBytes int64 = 100 << 20

// maxDirectBytes is the largest file sent with the import request itself.
// /doc caps a request's body at 8 MiB; the form around the file takes some.
const maxDirectBytes int64 = 7 << 20

// uploadExpiry is how long a large file's upload address stays good.
const uploadExpiry = time.Hour

func maxImportBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("DOC_IMPORT_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxImportBytes
}

// exportableFormats are the formats an export may be recorded in: the
// server's own, and the browser's PDF and Word files.
var exportableFormats = map[string]bool{"md": true, "html": true, "json": true, "pdf": true, "docx": true}

type pipeline struct {
	mu        sync.RWMutex
	importers []Importer
	exporters map[string]Exporter // kind/format
}

func newPipeline() *pipeline { return &pipeline{exporters: map[string]Exporter{}} }

// thePipeline is the pipeline the worker runs; Pipeline is it, unless a test
// swapped Pipeline for a fake.
var thePipeline = Pipeline.(*pipeline)

func (p *pipeline) RegisterImporter(i Importer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, have := range p.importers {
		if have.Format() == i.Format() {
			panic(fmt.Sprintf("content io: two importers for %q", i.Format()))
		}
	}
	p.importers = append(p.importers, i)
}

func (p *pipeline) RegisterExporter(e Exporter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := e.Kind() + "/" + e.Format()
	if _, ok := p.exporters[key]; ok {
		panic(fmt.Sprintf("content io: two exporters for %s", key))
	}
	p.exporters[key] = e
}

// importerFor is the importer that reads a file, by its name first.
func (p *pipeline) importerFor(filename, mediaType string) Importer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, i := range p.importers {
		if i.Accepts(filename, mediaType) {
			return i
		}
	}
	return nil
}

func (p *pipeline) importerByFormat(format string) Importer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, i := range p.importers {
		if i.Format() == format {
			return i
		}
	}
	return nil
}

// checkParent refuses an import under a page who can't edit.
func checkParent(ctx context.Context, who accessBusiness.Subject, parentUUID string) error {
	if parentUUID == "" {
		return nil
	}
	if _, err := uuid.Parse(parentUUID); err != nil {
		return ErrNotFound
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: parentUUID})
	if errors.Is(err, docBusiness.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Deleted || d.Level == accessBusiness.LevelNone {
		return ErrNotFound
	}
	if d.Level < accessBusiness.LevelEdit {
		return ErrForbidden
	}
	return nil
}

// checkRead refuses an export of a resource who can't read.
func checkRead(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) error {
	if _, err := uuid.Parse(res.UUID); err != nil {
		return ErrNotFound
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, res)
	if errors.Is(err, docBusiness.ErrNotFound) || errors.Is(err, accessBusiness.ErrUnknownKind) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Deleted || d.Level < accessBusiness.LevelView {
		return ErrNotFound
	}
	return nil
}

func inputKey(id uuid.UUID, filename string) string {
	name := uploadsafe.SanitiseFileName(path.Base(filename), 120)
	if name == "" {
		name = "file"
	}
	return "content-io/" + id.String() + "/" + name
}

func (p *pipeline) prepare(ctx context.Context, who accessBusiness.Subject, req ImportRequest) (Importer, uuid.UUID, error) {
	userID, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return nil, uuid.Nil, ErrForbidden
	}
	imp := p.importerFor(req.Filename, req.MediaType)
	if imp == nil {
		return nil, uuid.Nil, ErrUnsupported
	}
	if req.Size > maxImportBytes() {
		return nil, uuid.Nil, ErrTooLarge
	}
	if err := checkParent(ctx, who, req.ParentUUID); err != nil {
		return nil, uuid.Nil, err
	}
	return imp, userID, nil
}

func paramsOf(req ImportRequest, size int64) json.RawMessage {
	raw, _ := json.Marshal(jobParams{
		Filename: path.Base(req.Filename), MediaType: req.MediaType, ParentUUID: req.ParentUUID, Size: size,
	})
	return raw
}

func (p *pipeline) Import(ctx context.Context, who accessBusiness.Subject, req ImportRequest) (Job, error) {
	imp, userID, err := p.prepare(ctx, who, req)
	if err != nil {
		return Job{}, err
	}
	if req.Body == nil {
		return Job{}, ErrNotFound
	}
	max := maxImportBytes()
	id := uuid.New()
	key := inputKey(id, req.Filename)
	n, err := store.Put(ctx, key, io.LimitReader(req.Body, max+1), -1, "application/octet-stream")
	if err != nil {
		return Job{}, err
	}
	if n > max {
		_ = store.Remove(context.Background(), key)
		return Job{}, ErrTooLarge
	}
	row := &contentIOModels.Job{
		ID: id, Kind: "import", Format: imp.Format(), UserUUID: userID, Status: contentIOModels.StatusQueued,
		Params: paramsOf(req, n), InputKey: &key,
	}
	if err := jobs.Create(ctx, row); err != nil {
		_ = store.Remove(context.Background(), key)
		return Job{}, err
	}
	return p.announce(ctx, id)
}

func (p *pipeline) PrepareUpload(ctx context.Context, who accessBusiness.Subject, req ImportRequest) (Job, Upload, error) {
	imp, userID, err := p.prepare(ctx, who, req)
	if err != nil {
		return Job{}, Upload{}, err
	}
	id := uuid.New()
	key := inputKey(id, req.Filename)
	url, err := store.PresignPut(ctx, key, uploadExpiry)
	if err != nil {
		return Job{}, Upload{}, err
	}
	row := &contentIOModels.Job{
		ID: id, Kind: "import", Format: imp.Format(), UserUUID: userID, Status: contentIOModels.StatusUploading,
		Params: paramsOf(req, req.Size), InputKey: &key,
	}
	if err := jobs.Create(ctx, row); err != nil {
		return Job{}, Upload{}, err
	}
	job, err := p.announce(ctx, id)
	return job, Upload{
		URL: url, Method: "PUT", Headers: map[string]string{"Content-Type": "application/octet-stream"},
		ExpiresIn: int64(uploadExpiry.Seconds()),
	}, err
}

// own is a job who started; ErrNotFound for anyone else's.
func own(ctx context.Context, who accessBusiness.Subject, id string) (*contentIOModels.Job, error) {
	jobID, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	row, err := jobs.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if row == nil || row.UserUUID.String() != who.UserUUID {
		return nil, ErrNotFound
	}
	return row, nil
}

func (p *pipeline) StartUploaded(ctx context.Context, who accessBusiness.Subject, id string) (Job, error) {
	row, err := own(ctx, who, id)
	if err != nil {
		return Job{}, err
	}
	if row.Status != contentIOModels.StatusUploading {
		if row.Status == contentIOModels.StatusFailed {
			return toJob(row), ErrConflict
		}
		// Asked twice: it is already on its way.
		return toJob(row), nil
	}
	if row.InputKey == nil {
		return Job{}, ErrNotFound
	}
	size, err := store.Size(ctx, *row.InputKey)
	if err != nil {
		return Job{}, ErrNotFound
	}
	if size > maxImportBytes() {
		_ = store.Remove(context.Background(), *row.InputKey)
		msg := tooLargeMessage(size)
		_ = jobs.Finish(ctx, row.ID, contentIOModels.StatusFailed, nil, nil, &msg)
		return Job{}, ErrTooLarge
	}
	moved, err := jobs.Queue(ctx, row.ID, *row.InputKey)
	if err != nil {
		return Job{}, err
	}
	if !moved {
		return toJob(row), nil
	}
	return p.announce(ctx, row.ID)
}

// announce reads a job back and tells its owner's open tabs.
func (p *pipeline) announce(ctx context.Context, id uuid.UUID) (Job, error) {
	row, err := jobs.Get(ctx, id)
	if err != nil || row == nil {
		return Job{}, err
	}
	job := toJob(row)
	publishJob(job)
	return job, nil
}

func (p *pipeline) Job(ctx context.Context, who accessBusiness.Subject, id string) (Job, error) {
	row, err := own(ctx, who, id)
	if err != nil {
		return Job{}, err
	}
	return toJob(row), nil
}

func (p *pipeline) exporter(kind, format string) Exporter {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exporters[kind+"/"+format]
}

func (p *pipeline) Export(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format string, to ExportTarget) error {
	return p.export(ctx, who, res, format, "server", to)
}

// export writes res for who and records it as made via ("server", "api").
func (p *pipeline) export(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format, via string, to ExportTarget) error {
	exp := p.exporter(res.Kind, format)
	if exp == nil {
		return ErrUnsupported
	}
	if err := checkRead(ctx, who, res); err != nil {
		return err
	}
	if err := exp.Export(ctx, who, res, to); err != nil {
		return err
	}
	emitExported(ctx, who, res, format, via)
	return nil
}

func (p *pipeline) RecordExport(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format string) error {
	if !exportableFormats[format] {
		return ErrUnsupported
	}
	if err := checkRead(ctx, who, res); err != nil {
		return err
	}
	emitExported(ctx, who, res, format, "browser")
	return nil
}

func emitExported(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, format, via string) {
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind:         resourceEventBusiness.DocExported,
		ResourceKind: res.Kind,
		ResourceUUID: res.UUID,
		Actor:        resourceEventBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		At:           time.Now(),
		Data:         map[string]string{"format": format, "via": via},
	})
}

// extensioned is an importer that can name the files it reads.
type extensioned interface {
	Extensions() []string
}

func (p *pipeline) Capabilities(ctx context.Context, _ accessBusiness.Subject, isAdmin bool) Capabilities {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c := Capabilities{
		Export:         isAdmin || policyBusiness.Policies.Bool(ctx, policyBusiness.DocsExportMembers),
		MaxImportBytes: maxImportBytes(),
		MaxDirectBytes: maxDirectBytes,
	}
	for key := range p.exporters {
		if kind, format, ok := strings.Cut(key, "/"); ok && kind == accessBusiness.KindDoc {
			c.ExportFormats = append(c.ExportFormats, format)
		}
	}
	sort.Strings(c.ExportFormats)
	for _, i := range p.importers {
		if e, ok := i.(extensioned); ok {
			c.ImportExtensions = append(c.ImportExtensions, e.Extensions()...)
		}
	}
	return c
}

func tooLargeMessage(size int64) string {
	return fmt.Sprintf("The file is %s; imports take files up to %s.", helpers.ReadableBytes(size), helpers.ReadableBytes(maxImportBytes()))
}
