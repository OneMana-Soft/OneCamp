package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// The pipeline's seams, faked: the job table, object storage, the access
// rules, doc creation, the asset uploader, the Live Doc Writer and the event
// bus. world records what the pipeline did to each.
type world struct {
	t        *testing.T
	mu       sync.Mutex
	jobs     map[uuid.UUID]*contentIOModels.Job
	files    map[string][]byte
	levels   map[string]accessBusiness.Level // doc → the tester's level
	owned    map[string]bool                 // docs the tester owns
	docs     []fakeDoc
	seeded   map[string]string // doc → seeded HTML
	quiet    map[string]bool   // doc → whether its seed was quiet
	assets   []string          // uploaded asset names
	appended []appendCall      // what the live writer was asked to append
	deleted  []string
	events   []resourceEventBusiness.Event
	seedErr  error
	bodies   map[string][2]string // doc → title, stored HTML (for exports)
	// failTitles are docs whose seed is refused; seeds counts the seeds asked,
	// and past downAfter of them (when it is set) the service is down.
	failTitles map[string]bool
	seeds      int
	downAfter  int
	// discardErr is what removing a doc answers.
	discardErr error
}

type fakeDoc struct {
	uuid, title, icon, parent string
}

var tester = accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()}

func newWorld(t *testing.T) *world {
	w := &world{
		t: t, jobs: map[uuid.UUID]*contentIOModels.Job{}, files: map[string][]byte{},
		levels: map[string]accessBusiness.Level{}, owned: map[string]bool{}, seeded: map[string]string{}, quiet: map[string]bool{}, bodies: map[string][2]string{},
	}
	saved := struct {
		jobs    jobStore
		store   fileStore
		rules   accessBusiness.Resolver
		create  func(context.Context, *dgraphStruct.DgraphUser, accessBusiness.Subject, string, string, string) (string, error)
		discard func(context.Context, *dgraphStruct.DgraphUser, accessBusiness.Subject, string) error
		upload  func(context.Context, uuid.UUID, string, string, io.Reader, int64) (string, string, error)
		live    collab.Writer
		bus     resourceEventBusiness.Emitter
		subject func(context.Context, string) (accessBusiness.Subject, *dgraphStruct.DgraphUser, error)
		load    func(context.Context, accessBusiness.Subject, string) (string, string, error)
		publish func(Job)
	}{jobs, store, accessBusiness.Rules, createDoc, discardDoc, uploadAsset, collab.Live, resourceEventBusiness.Bus, subjectFor, loadDoc, publishJob}
	t.Cleanup(func() {
		jobs, store, accessBusiness.Rules, createDoc, discardDoc = saved.jobs, saved.store, saved.rules, saved.create, saved.discard
		uploadAsset, collab.Live, resourceEventBusiness.Bus, subjectFor = saved.upload, saved.live, saved.bus, saved.subject
		loadDoc, publishJob = saved.load, saved.publish
	})
	jobs = fakeJobs{w}
	store = fakeStore{w}
	accessBusiness.Rules = fakeRules{w}
	createDoc = func(_ context.Context, _ *dgraphStruct.DgraphUser, _ accessBusiness.Subject, title, icon, parent string) (string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		id := uuid.NewString()
		w.docs = append(w.docs, fakeDoc{uuid: id, title: title, icon: icon, parent: parent})
		// As the real ones do (A7's default, and A3's create once wired).
		w.events = append(w.events, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocCreated, ResourceKind: "doc", ResourceUUID: id})
		return id, nil
	}
	discardDoc = func(_ context.Context, _ *dgraphStruct.DgraphUser, _ accessBusiness.Subject, id string) error {
		if w.discardErr != nil {
			return w.discardErr
		}
		w.deleted = append(w.deleted, id)
		return nil
	}
	uploadAsset = func(_ context.Context, _ uuid.UUID, docUUID, name string, r io.Reader, _ int64) (string, string, error) {
		data, _ := io.ReadAll(r)
		if len(data) == 0 {
			return "", "", errors.New("empty")
		}
		w.assets = append(w.assets, name)
		obj := uuid.NewString()
		return obj, "https://api.example.test/doc/getDocAttachment/" + docUUID + "/" + obj, nil
	}
	collab.Live = fakeLive{w}
	resourceEventBusiness.Bus = fakeBus{w}
	subjectFor = func(context.Context, string) (accessBusiness.Subject, *dgraphStruct.DgraphUser, error) {
		return tester, &dgraphStruct.DgraphUser{Uid: tester.UserUID, Uuid: tester.UserUUID}, nil
	}
	loadDoc = func(_ context.Context, _ accessBusiness.Subject, id string) (string, string, error) {
		b, ok := w.bodies[id]
		if !ok {
			return "", "", ErrNotFound
		}
		return b[0], b[1], nil
	}
	publishJob = func(Job) {}
	return w
}

// importFile runs a file through an import, start to end, and returns its job.
func (w *world) importFile(name string, data []byte, parent string) Job {
	w.t.Helper()
	job, err := Pipeline.Import(context.Background(), tester, ImportRequest{
		Filename: name, ParentUUID: parent, Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil {
		w.t.Fatalf("import of %s: %v", name, err)
	}
	if err := runQueued(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	done, err := Pipeline.Job(context.Background(), tester, job.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return done
}

type fakeJobs struct{ w *world }

func (f fakeJobs) Create(_ context.Context, j *contentIOModels.Job) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	c := *j
	c.CreatedAt, c.UpdatedAt = time.Now(), time.Now()
	if c.Result == nil {
		c.Result = json.RawMessage("{}")
	}
	f.w.jobs[j.ID] = &c
	return nil
}

func (f fakeJobs) Get(_ context.Context, id uuid.UUID) (*contentIOModels.Job, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	j, ok := f.w.jobs[id]
	if !ok {
		return nil, nil
	}
	c := *j
	return &c, nil
}

func (f fakeJobs) Queue(_ context.Context, id uuid.UUID, key string) (bool, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	j := f.w.jobs[id]
	if j == nil || j.Status != contentIOModels.StatusUploading {
		return false, nil
	}
	j.Status, j.InputKey = contentIOModels.StatusQueued, &key
	return true, nil
}

func (f fakeJobs) ClaimNext(_ context.Context) (*contentIOModels.Job, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	var queued []*contentIOModels.Job
	for _, j := range f.w.jobs {
		if j.Status == contentIOModels.StatusQueued {
			queued = append(queued, j)
		}
	}
	if len(queued) == 0 {
		return nil, nil
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].CreatedAt.Before(queued[b].CreatedAt) })
	queued[0].Status = contentIOModels.StatusRunning
	c := *queued[0]
	return &c, nil
}

func (f fakeJobs) SetProgress(_ context.Context, id uuid.UUID, p int) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.jobs[id].Progress = p
	return nil
}

func (f fakeJobs) Finish(_ context.Context, id uuid.UUID, status string, result json.RawMessage, outputKey, errText *string) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	j := f.w.jobs[id]
	now := time.Now()
	j.Status, j.Result, j.Error, j.FinishedAt, j.OutputKey = status, result, errText, &now, outputKey
	if status == contentIOModels.StatusDone {
		j.Progress = 100
	}
	return nil
}

func (f fakeJobs) Ready(_ context.Context, id uuid.UUID, result json.RawMessage) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	j := f.w.jobs[id]
	if j.Status == contentIOModels.StatusRunning {
		j.Status, j.Progress, j.Result = contentIOModels.StatusReady, 100, result
	}
	return nil
}

func (f fakeJobs) Choose(_ context.Context, id uuid.UUID, params json.RawMessage) (bool, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	j := f.w.jobs[id]
	if j == nil || j.Status != contentIOModels.StatusReady {
		return false, nil
	}
	j.Status, j.Params, j.Result, j.Progress = contentIOModels.StatusQueued, params, json.RawMessage("{}"), 0
	return true, nil
}

func (f fakeJobs) FailAbandoned(context.Context, time.Time, time.Time, time.Time) ([]*contentIOModels.Job, error) {
	return nil, nil
}

func (f fakeJobs) FinishedBefore(context.Context, time.Time, int) ([]*contentIOModels.Job, error) {
	return nil, nil
}

func (f fakeJobs) ForgetFiles(_ context.Context, id uuid.UUID) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.jobs[id].InputKey = nil
	return nil
}

func (f fakeJobs) DeleteFinishedBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (f fakeJobs) Recent(_ context.Context, user uuid.UUID, kind, format string, limit int) ([]*contentIOModels.Job, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	var out []*contentIOModels.Job
	for _, j := range f.w.jobs {
		if j.UserUUID == user && j.Kind == kind && j.Format == format {
			c := *j
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out[:min(limit, len(out))], nil
}

type fakeStore struct{ w *world }

func (f fakeStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.files[key] = data
	return int64(len(data)), nil
}

func (f fakeStore) Fetch(_ context.Context, key string, max int64) (*os.File, int64, error) {
	f.w.mu.Lock()
	data, ok := f.w.files[key]
	f.w.mu.Unlock()
	if !ok {
		return nil, 0, ErrNotFound
	}
	if int64(len(data)) > max {
		return nil, int64(len(data)), ErrTooLarge
	}
	tmp, err := os.CreateTemp(f.w.t.TempDir(), "in-*")
	if err != nil {
		return nil, 0, err
	}
	_, _ = tmp.Write(data)
	return tmp, int64(len(data)), nil
}

func (f fakeStore) Size(_ context.Context, key string) (int64, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	data, ok := f.w.files[key]
	if !ok {
		return 0, ErrNotFound
	}
	return int64(len(data)), nil
}

func (f fakeStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://minio.example.test/" + key + "?signed", nil
}

func (f fakeStore) PresignGet(_ context.Context, key string, _ time.Duration, filename string) (string, error) {
	return "https://storage.example.test/" + key + "?as=" + filename, nil
}

func (f fakeStore) Remove(_ context.Context, key string) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	delete(f.w.files, key)
	return nil
}

type fakeRules struct{ w *world }

func (f fakeRules) Resolve(_ context.Context, _ accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	l, ok := f.w.levels[res.UUID]
	if !ok {
		return accessBusiness.Decision{}, docBusiness.ErrNotFound
	}
	return accessBusiness.Decision{Level: l, Owner: f.w.owned[res.UUID]}, nil
}

func (f fakeRules) Filter(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource, atLeast accessBusiness.Level) ([]accessBusiness.Resource, error) {
	var kept []accessBusiness.Resource
	for _, r := range res {
		if l, ok := f.w.levels[r.UUID]; ok && l >= atLeast {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

func (f fakeRules) ExplainChange(context.Context, accessBusiness.Subject, accessBusiness.Change) (accessBusiness.Explanation, error) {
	return accessBusiness.Explanation{}, accessBusiness.ErrUnavailable
}

func (f fakeRules) CopyOnCreate(context.Context, accessBusiness.Subject, accessBusiness.Resource, ...accessBusiness.Resource) error {
	return nil
}

func (f fakeRules) ApplyToSubtree(context.Context, accessBusiness.Subject, accessBusiness.Change) error {
	return accessBusiness.ErrUnavailable
}

func (f fakeRules) Transfer(context.Context, accessBusiness.Subject, accessBusiness.Resource, string) error {
	return accessBusiness.ErrUnavailable
}

type fakeLive struct{ w *world }

func (f fakeLive) Seed(_ context.Context, id, html string, o collab.Options) (collab.Result, error) {
	f.w.seeds++
	if f.w.seedErr != nil {
		return collab.Result{}, f.w.seedErr
	}
	if f.w.downAfter > 0 && f.w.seeds > f.w.downAfter {
		return collab.Result{}, collab.ErrUnavailable
	}
	for _, d := range f.w.docs {
		if d.uuid == id && f.w.failTitles[d.title] {
			return collab.Result{}, errors.New("live write: refused")
		}
	}
	if o.By.UserUUID != tester.UserUUID {
		f.w.t.Errorf("a seed must be attributed to the person importing, got %+v", o.By)
	}
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.seeded[id] = html
	f.w.quiet[id] = o.Quiet
	return collab.Result{}, nil
}

func (f fakeLive) Append(_ context.Context, id, html string, o collab.Options) (collab.Result, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.appended = append(f.w.appended, appendCall{doc: id, html: html, opts: o})
	return collab.Result{Blocks: strings.Count(html, "<p>")}, nil
}

// appendCall is one Append the fake writer took.
type appendCall struct {
	doc, html string
	opts      collab.Options
}

func (fakeLive) InsertAfter(context.Context, string, string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, collab.ErrUnavailable
}
func (fakeLive) ReplaceBlocks(context.Context, string, []string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, collab.ErrUnavailable
}
func (fakeLive) ReplaceAll(context.Context, string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, collab.ErrUnavailable
}
func (fakeLive) Reauth(context.Context, string, []string) error { return collab.ErrUnavailable }
func (fakeLive) ApplyProposal(context.Context, string, string, []string, collab.Options) (collab.Result, error) {
	return collab.Result{}, collab.ErrUnavailable
}

type fakeBus struct{ w *world }

func (f fakeBus) Emit(_ context.Context, e resourceEventBusiness.Event) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.events = append(f.w.events, e)
}

// docTitled is the doc made with this title.
func (w *world) docTitled(title string) fakeDoc {
	w.t.Helper()
	for _, d := range w.docs {
		if d.title == title {
			return d
		}
	}
	var titles []string
	for _, d := range w.docs {
		titles = append(titles, d.title)
	}
	w.t.Fatalf("no doc titled %q; made: %s", title, strings.Join(titles, ", "))
	return fakeDoc{}
}
