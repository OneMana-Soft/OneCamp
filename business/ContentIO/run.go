package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
)

// The worker: a sweep that drains the queue, and one that tidies up after it.
// The queue's sweep runs on every instance (ClaimsItsOwnWork): a job is taken
// with FOR UPDATE SKIP LOCKED, so instances share the queue between them. The
// cleanup, which only fails jobs nobody has touched for half an hour, is the
// Sweeper's to claim: one instance a pass.
func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "content-io.run", Every: QueuePollEvery, Run: runQueued, ClaimsItsOwnWork: true})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "content-io.cleanup", Every: time.Hour, Run: cleanup})
}

// QueuePollEvery is how often each worker looks for queued jobs: a job queued
// while a worker is idle is taken within one interval.
const QueuePollEvery = 2 * time.Second

// runBudget is how long one pass keeps taking jobs before it yields.
const runBudget = 50 * time.Second

// maxPages is the most docs one import makes.
const maxPages = 500

func runQueued(ctx context.Context) error {
	until := time.Now().Add(runBudget)
	for time.Now().Before(until) && ctx.Err() == nil {
		row, err := jobs.ClaimNext(ctx)
		if err != nil {
			return err
		}
		if row == nil {
			return nil
		}
		thePipeline.run(ctx, row)
	}
	return nil
}

// keepFilesFor is how long a job's files are kept once it ends: an import's
// upload, and the zip an export made.
const keepFilesFor = 24 * time.Hour

// cleanup fails what was abandoned and removes what is no longer needed: a
// job's files once keepFilesFor has passed since it ended, the job itself
// after a month.
func cleanup(ctx context.Context) error {
	now := time.Now()
	dead, err := jobs.FailAbandoned(ctx, now.Add(-30*time.Minute), now.Add(-2*uploadExpiry))
	if err != nil {
		return err
	}
	for _, row := range dead {
		publishJob(toJob(row))
	}
	old, err := jobs.FinishedBefore(ctx, now.Add(-keepFilesFor), 200)
	if err != nil {
		return err
	}
	for _, row := range append(dead, old...) {
		for _, key := range []*string{row.InputKey, row.OutputKey} {
			if key != nil {
				_ = store.Remove(ctx, *key)
			}
		}
		_ = jobs.ForgetFiles(ctx, row.ID)
	}
	_, err = jobs.DeleteFinishedBefore(ctx, now.Add(-30*24*time.Hour))
	return err
}

// subjectFor is the person who started a job, as access decisions and doc
// creation need them. A person removed since can't import any more.
var subjectFor = func(ctx context.Context, userUUID string) (accessBusiness.Subject, *dgraphStruct.DgraphUser, error) {
	user, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil {
		return accessBusiness.Subject{}, nil, err
	}
	if user == nil || user.Uid == "" {
		return accessBusiness.Subject{}, nil, ErrForbidden
	}
	return accessBusiness.Subject{UserUID: user.Uid, UserUUID: userUUID}, user, nil
}

// run does one import, start to end. It never returns an error: the job
// records how it ended.
func (p *pipeline) run(ctx context.Context, row *contentIOModels.Job) {
	if row.Kind == "export" {
		p.runWorkspaceExport(ctx, row)
		return
	}
	var params jobParams
	_ = json.Unmarshal(row.Params, &params)
	var created []Created
	var skipped []Skipped
	finish := func(msg string) {
		result, _ := json.Marshal(jobResult{Created: created, Skipped: skipped})
		status := contentIOModels.StatusDone
		var errText *string
		if msg != "" {
			status = contentIOModels.StatusFailed
			errText = &msg
		}
		if err := jobs.Finish(ctx, row.ID, status, result, nil, errText); err != nil {
			helpers.LogErrorWithContext(ctx, "content io: job %s could not be finished: %v", row.ID, err)
		}
		if row.InputKey != nil {
			if err := store.Remove(context.Background(), *row.InputKey); err == nil {
				_ = jobs.ForgetFiles(ctx, row.ID)
			}
		}
		_, _ = p.announce(ctx, row.ID)
	}

	imp := p.importerByFormat(row.Format)
	if imp == nil {
		finish("This kind of file can't be imported any more.")
		return
	}
	who, user, err := subjectFor(ctx, row.UserUUID.String())
	if err != nil {
		finish("The person who started this import can't make docs any more.")
		return
	}
	// Access may have changed while the job waited.
	if err := checkParent(ctx, who, params.ParentUUID); err != nil {
		finish("You can no longer add pages to the page this import was going under.")
		return
	}
	if row.InputKey == nil {
		finish("The file is gone: import it again.")
		return
	}
	f, size, err := store.Fetch(ctx, *row.InputKey, maxImportBytes())
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			finish(tooLargeMessage(size))
			return
		}
		finish("The file could not be read back from storage: import it again.")
		return
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	progress := newProgress(ctx, p, row)
	out := &collector{report: func(done, total int) { progress.set(scaled(done, total, 0, 40)) }}
	in := Input{Filename: params.Filename, MediaType: params.MediaType, Size: size, File: f}
	if err := imp.Import(ctx, in, out); err != nil {
		skipped = out.skipped
		finish(importError(ctx, err))
		return
	}
	if len(out.pages) == 0 {
		skipped = out.skipped
		finish("There was nothing in the file to make a doc from.")
		return
	}
	maker := &docMaker{
		who: who, user: user, userID: row.UserUUID, parent: params.ParentUUID,
		report: func(done, total int) { progress.set(scaled(done, total, 40, 100)) },
	}
	made := maker.make(ctx, out.pages)
	created, skipped = made.Created, append(out.skipped, made.Skipped...)
	if made.Filled == 0 {
		finish(nothingImported(len(created), skipped))
		return
	}
	finish("")
}

// nothingImported is what an import that filled no doc says: why, and any
// empty doc it could not take away again.
func nothingImported(leftEmpty int, skipped []Skipped) string {
	switch {
	case leftEmpty == 1:
		return "Nothing could be imported: the document service could not take the content. The empty doc it made could not be removed; delete it, then try again."
	case leftEmpty > 1:
		return fmt.Sprintf("Nothing could be imported: the document service could not take the content. The %d empty docs it made could not be removed; delete them, then try again.", leftEmpty)
	}
	for _, s := range skipped {
		if s.Why == whyNotFilled || s.Why == whyNotMade {
			return "No doc could be made: " + s.Why + "."
		}
	}
	return "No doc could be made."
}

// importError is what an importer's error says to the person importing.
func importError(ctx context.Context, err error) string {
	var said readError
	if errors.As(err, &said) {
		return said.msg
	}
	helpers.LogWarnWithContext(ctx, "content io: an import failed: %v", err)
	return "The file could not be read: it may be damaged, or not what its name says."
}

// readError is an importer's error worded for the person importing.
type readError struct{ msg string }

func (e readError) Error() string { return e.msg }

func scaled(done, total, from, to int) int {
	if total <= 0 {
		return from
	}
	if done > total {
		done = total
	}
	return from + (to-from)*done/total
}

// progress records a running job's progress, at most twice a second.
type progress struct {
	mu   sync.Mutex
	ctx  context.Context
	p    *pipeline
	row  *contentIOModels.Job
	last time.Time
	pct  int
}

func newProgress(ctx context.Context, p *pipeline, row *contentIOModels.Job) *progress {
	return &progress{ctx: ctx, p: p, row: row}
}

func (pr *progress) set(pct int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pct <= pr.pct || time.Since(pr.last) < 500*time.Millisecond {
		return
	}
	pr.pct, pr.last = pct, time.Now()
	if err := jobs.SetProgress(pr.ctx, pr.row.ID, pct); err == nil {
		job := toJob(pr.row)
		job.Status, job.Progress = JobRunning, pct
		publishJob(job)
	}
}

// collector keeps what an importer finds until the docs are made.
type collector struct {
	pages   []Page
	keys    map[string]bool
	skipped []Skipped
	report  func(done, total int)
}

func (c *collector) Page(p Page) error {
	if p.Body == nil {
		return nil
	}
	if len(c.pages) >= maxPages {
		return readError{fmt.Sprintf("An import makes at most %d docs; split the file and import the rest.", maxPages)}
	}
	if c.keys == nil {
		c.keys = map[string]bool{}
	}
	if p.Key == "" || c.keys[p.Key] {
		p.Key = fmt.Sprintf("%s#%d", p.Key, len(c.pages)+1)
	}
	c.keys[p.Key] = true
	c.pages = append(c.pages, p)
	return nil
}

func (c *collector) Progress(done, total int) {
	if c.report != nil {
		c.report(done, total)
	}
}

func (c *collector) Skip(what, why string) {
	c.skipped = append(c.skipped, Skipped{What: what, Why: why})
}
