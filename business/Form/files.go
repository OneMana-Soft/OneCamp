package business

// Files a visitor sends with a form. A file question's page uploads each file
// as it is picked (POST /public/form/{token}/file): the file is checked and
// kept through the one upload store (attachmentBusiness.StoreUpload), refusing what a
// browser could run and refusing when the scanner can't say, and it waits in
// form_uploads under a claim the page holds. When the form is sent, the files
// the answers name are taken under that claim, become the project's
// attachments in the same statement, and go on the task. A file whose form
// was never sent is removed a day later by a claimed sweep.
//
// Nothing a visitor sends acts with a member's reach: a file is only ever one
// they uploaded under their own claim, so an answer naming another file (the
// owner's, another visitor's) attaches nothing.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/google/uuid"
)

const (
	// maxFileQuestions is how many questions of a form take files, and
	// maxFilesPerQuestion how many files each takes.
	maxFileQuestions    = 3
	maxFilesPerQuestion = 5
	// maxVisitorFileMB is the largest file a visitor sends, unless the
	// workspace's own upload limit is smaller.
	maxVisitorFileMB = 10
	// maxUploadsPerFormPerHour is the brake on a form's files being flooded.
	maxUploadsPerFormPerHour = 200
	// uploadsKeptFor is how long a file waits for its form to be sent: hours,
	// not a day, so a stranger's files never stand long (code review, 22 Oct).
	uploadsKeptFor = 6 * time.Hour
	// maxUnsentPerForm and maxUnsentInAll bound the bytes waiting unsent, for
	// one form and for every form, so files nobody sends can't fill a small
	// install's disk.
	maxUnsentPerForm = 500 << 20
	maxUnsentInAll   = 2 << 30
	// purgeGiveUp is how many sweeps try to remove an object before its row is
	// forgotten and the object named in the log as left in storage.
	purgeGiveUp = 8
	// sweepUploads is the claimed sweep that removes files never sent.
	sweepUploads = "forms.uploads.purge"
)

// FileRef is a file an answer carries: its id, and once sent its name, size
// and type.
type FileRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mime string `json:"mime,omitempty"`
}

// FileLimits is what a visitor may send to a file question.
type FileLimits struct {
	Max int   `json:"max"`
	MB  int64 `json:"mb"`
}

// Uploaded is a visitor's file as its upload answers it: the claim their page
// holds from now on, and the file.
type Uploaded struct {
	Claim string  `json:"claim"`
	File  FileRef `json:"file"`
}

// Seams for tests: where a file is kept, and where it waits.
var (
	storeFile        = attachmentBusiness.StoreUpload
	unstoreFiles     = attachmentBusiness.RemoveStoredAll
	addUpload        = formModel.AddUpload
	waitingFiles     = formModel.Waiting
	uploadsSince     = formModel.UploadsSince
	claimUploads     = formModel.Claim
	releaseUploads   = formModel.Release
	unsentUploads    = formModel.Unsent
	forgetUploads    = formModel.Forget
	triedUploads     = formModel.Tried
	unsentBytes      = formModel.UnsentBytes
	workspaceLimitMB = settingsBusiness.UploadLimitMB
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: sweepUploads, Every: time.Hour, Run: func(ctx context.Context) error {
		return purgeUnsent(ctx, time.Now())
	}})
}

// FileLimitMB is the largest file a visitor may send, in MB: 10, or the
// workspace's upload limit when that is smaller.
func FileLimitMB() int64 {
	return min(maxVisitorFileMB, workspaceLimitMB())
}

// newClaim is a claim for a visitor's files: 32 random bytes, as hex.
func newClaim() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// claimHash is how a claim is kept: its SHA-256, so the table never holds
// what lets someone send the files. Pure.
func claimHash(claim string) string {
	sum := sha256.Sum256([]byte(claim))
	return hex.EncodeToString(sum[:])
}

// isClaim says whether a claim is one newClaim makes. Pure.
func isClaim(claim string) bool {
	if len(claim) != 64 {
		return false
	}
	_, err := hex.DecodeString(claim)
	return err == nil
}

// UploadVisitorFile keeps a file a visitor adds to one of a form's file
// questions, until the form is sent with it: under their claim (a new one
// when they have none yet), at most five a question, 10 MB a file (or the
// workspace's limit), refusing what a browser could run, and failing closed
// when the scanner can't say whether it is clean.
func UploadVisitorFile(ctx context.Context, token, claim, question string, body io.Reader, size int64, name string, now time.Time) (*Uploaded, error) {
	l, err := loadLive(ctx, token)
	if err != nil {
		return nil, err
	}
	var q *Field
	for i := range l.fields {
		if l.fields[i].Id == question && l.fields[i].Type == File {
			q = &l.fields[i]
		}
	}
	if q == nil {
		return nil, &FormError{"That question doesn't take files."}
	}
	if limit := FileLimitMB(); size > limit<<20 {
		return nil, &FormError{fmt.Sprintf("Files can be up to %d MB.", limit)}
	}
	if claim == "" {
		if claim, err = newClaim(); err != nil {
			return nil, err
		}
	} else if !isClaim(claim) {
		return nil, &FormError{"Your files didn't come through. Reload the form and add them again."}
	}
	hash := claimHash(claim)
	full := &FormError{fmt.Sprintf("Add at most %d files to “%s”.", maxFilesPerQuestion, q.Label)}
	if n, err := waitingFiles(ctx, l.form.Id, hash, q.Id); err != nil {
		return nil, err
	} else if n >= maxFilesPerQuestion {
		return nil, full
	}
	if n, err := uploadsSince(ctx, l.form.Id, now.Add(-time.Hour)); err != nil {
		return nil, err
	} else if n >= maxUploadsPerFormPerHour {
		return nil, &FormError{"This form is taking a lot of files right now. Try again in a while."}
	}
	if forForm, forAll, err := unsentBytes(ctx, l.form.Id); err != nil {
		return nil, err
	} else if forForm+size > maxUnsentPerForm || forAll+size > maxUnsentInAll {
		return nil, &FormError{"This form is taking a lot of files right now. Try again in a while."}
	}
	// A stranger's file, in no one's folder until the form is sent: what a
	// browser could run is refused, not defused, and a scanner that can't
	// say refuses it too.
	stored, err := storeFile(ctx, body, size, name, uuid.Nil, attachmentBusiness.StoreOptions{
		Safe:       uploadsafe.Options{AllowInlineImages: true, RefuseRunnable: true},
		Folder:     fmt.Sprintf("forms/%s", l.form.Id),
		FailClosed: true,
	})
	if err != nil {
		return nil, err
	}
	id := stored.ID
	ok, err := addUpload(ctx, formModel.Upload{ID: id, Form: l.form.Id, Claim: hash, Question: q.Id, Key: stored.ObjKey,
		Name: stored.SafeName, ContentType: stored.ContentType, Size: stored.Size}, maxFilesPerQuestion)
	if err != nil || !ok {
		if rerr := unstoreFiles(context.WithoutCancel(ctx), []string{stored.ObjKey}); rerr != nil {
			helpers.LogWarnWithContext(ctx, "business/Form UploadVisitorFile unstore err: %+v", rerr)
		}
		if err != nil {
			return nil, err
		}
		return nil, full
	}
	return &Uploaded{Claim: claim, File: FileRef{ID: id.String(), Name: stored.SafeName, Size: stored.Size, Mime: stored.ContentType}}, nil
}

// taken is the files a submission took: as the task's attachments, and their
// ids, to put back if the task can't be made.
type taken struct {
	attachments []*dgraphStruct.DgraphAttachment
	ids         []uuid.UUID
	project     uuid.UUID
}

// release puts the files back to waiting, so sending again works.
func (t taken) release(ctx context.Context) {
	if len(t.ids) == 0 {
		return
	}
	if err := releaseUploads(context.WithoutCancel(ctx), t.ids, t.project); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Form release files err: %+v", err)
	}
}

// claimFiles takes the files the answers name, under the visitor's claim, as
// the project's attachments owned by the form's owner (who files the task),
// and writes each file answer with its files' names. A file that isn't
// waiting under the claim (gone after a day, sent already, or never theirs)
// fails the submission: the visitor adds it again.
func claimFiles(ctx context.Context, l *live, claim string, answers []Answer) (taken, error) {
	out := taken{project: l.form.ProjectUUID}
	var ids []uuid.UUID
	for _, a := range answers {
		for _, f := range a.Files {
			if id, err := uuid.Parse(f.ID); err == nil {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	gone := &FormError{"A file you added isn't there any more. Add it again, then send."}
	if !isClaim(claim) {
		return out, gone
	}
	got, err := claimUploads(ctx, l.form.Id, claimHash(claim), ids, l.form.ProjectUUID, l.form.CreatedBy)
	if err != nil {
		return out, err
	}
	byID := make(map[string]formModel.Upload, len(got))
	for _, u := range got {
		byID[u.ID.String()] = u
		out.ids = append(out.ids, u.ID)
	}
	if len(got) != len(ids) {
		out.release(ctx)
		return taken{}, gone
	}
	for i := range answers {
		if len(answers[i].Files) == 0 {
			continue
		}
		names := make([]string, 0, len(answers[i].Files))
		for j, f := range answers[i].Files {
			u := byID[f.ID]
			answers[i].Files[j] = FileRef{ID: f.ID, Name: u.Name, Size: u.Size, Mime: u.ContentType}
			names = append(names, u.Name)
			out.attachments = append(out.attachments, &dgraphStruct.DgraphAttachment{Uuid: f.ID, FileName: u.Name, ObjectKey: u.Key,
				Type: attachmentBusiness.KindOf(u.Name), RawType: u.ContentType, Size: int(u.Size)})
		}
		answers[i].Value, answers[i].Values = strings.Join(names, ", "), names
	}
	return out, nil
}

// purgeUnsent removes the files that waited for a form that was never sent
// with them, objects first: a row is forgotten once its object is gone, so a
// pass that can't reach storage at all leaves them for the next. One whose
// object won't go waits for its retry (helpers.Backoff, an hour doubling to a
// day), and after purgeGiveUp tries it's forgotten and its key logged, so one
// file that keeps failing never stops the sweep. Rows of files that were sent are
// forgotten too; their objects are the project's.
func purgeUnsent(ctx context.Context, now time.Time) error {
	before := now.Add(-uploadsKeptFor)
	unsent, err := unsentUploads(ctx, before, now, 500)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(unsent))
	for _, u := range unsent {
		keys = append(keys, u.Key)
	}
	failed := map[string]bool{}
	if err := unstoreFiles(ctx, keys); err != nil {
		var left *attachmentBusiness.RemoveError
		if !errors.As(err, &left) {
			return err
		}
		for _, k := range left.Failed {
			failed[k] = true
		}
	}
	gone, stuck, retry := make([]uuid.UUID, 0, len(unsent)), []uuid.UUID{}, []time.Time{}
	for _, u := range unsent {
		if failed[u.Key] {
			stuck, retry = append(stuck, u.ID), append(retry, now.Add(helpers.Backoff(u.Tries+1, time.Hour, 24*time.Hour)))
		} else {
			gone = append(gone, u.ID)
		}
	}
	giveUp, err := triedUploads(ctx, stuck, retry, purgeGiveUp)
	if err != nil {
		return err
	}
	if len(giveUp) > 0 {
		helpers.LogErrorWithContext(ctx, "business/Form purgeUnsent: %d unsent files left in storage after %d tries: %v", len(giveUp), purgeGiveUp, giveUp)
	}
	return forgetUploads(ctx, append(gone, giveUp...), before)
}
