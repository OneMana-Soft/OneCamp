package business

// An import read from Slack's API (api_reader.go) runs as an upload does. Its
// job comes first and waits, stage "reading", while the reader streams the
// export into the import's store, where an upload's export goes. Then it goes
// on to be planned and run as any import is. The admin watches the read (the
// job's progress, and the same live progress an import publishes) and can
// discard it, which stops it.
//
// The token is held in memory for the read alone: never stored, never logged.
// A read the server loses to a restart leaves its job waiting, and the
// cleanup's sweep of abandoned uploads fails it after its hours.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	slackapi "github.com/akashc777/OneCamp/services/Slack"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// APIRead is a read an admin asks for.
type APIRead struct {
	// Workspace labels the Slack workspace, as an upload's does.
	Workspace string
	// Token is a Slack user's (xoxp-) or an app's (xoxb-) token.
	Token   string
	Options APIReadOptions
}

// apiReadStage is the stage of a job whose export is being read.
const apiReadStage = "reading"

// StagedExportKey is where a job's export is staged in the import's store.
func StagedExportKey(jobId uuid.UUID) string {
	return fmt.Sprintf("slackImport/%s/raw.zip", jobId)
}

// Seams for tests.
var (
	// newAPIClient is the client a read calls Slack with: patient, since a
	// large workspace meets Slack's rate limits many times over.
	newAPIClient = func(token string) apiClient {
		return slackapi.Client{Token: token, Retries: 8, MaxWait: time.Minute, Timeout: 30 * time.Second}
	}
	// stageExport streams an export of a size not known yet into the store,
	// in 16 MB parts, so neither memory nor the server's disk holds it.
	stageExport = func(ctx context.Context, key string, r io.Reader) error {
		_, err := minioInit.MinioClient.PutObject(ctx, stagedZipBucket(), key, r, -1,
			minio.PutObjectOptions{ContentType: "application/zip", PartSize: 16 << 20})
		return err
	}
)

// StartAPIRead makes the job for read, asked for by the admin by, and starts
// reading into it. importModels.ErrConflictActiveJob is a workspace with an
// import under way.
func StartAPIRead(ctx context.Context, read APIRead, by uuid.UUID) (uuid.UUID, error) {
	jobId := uuid.New()
	key := StagedExportKey(jobId)
	job := &importModels.Job{
		Id:                 jobId,
		SlackWorkspaceName: read.Workspace,
		Source:             importModels.SourceAPI,
		RawObjectKey:       &key,
		Status:             importModels.StatusPending,
		TriggeredBy:        &by,
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		return uuid.Nil, err
	}
	if _, err := importModels.UpdateStatusFrom(ctx, jobId, []string{importModels.StatusPending},
		importModels.StatusPending, strPtr(apiReadStage), nil); err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport API read job=%s: naming its stage: %+v", jobId, err)
	}

	// The read outlives the request; discarding the job stops it.
	stop := make(chan struct{})
	registerActiveJob(jobId, stop)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	helpers.GoSafeNamed("business/SlackImport: the API read's stop", func() {
		select {
		case <-stop:
			cancel()
		case <-runCtx.Done():
		}
	})
	helpers.GoSafeNamed("business/SlackImport: read from Slack's API", func() {
		defer cancel()
		defer removeActiveJob(jobId)
		readAPIIntoJob(runCtx, jobId, read)
	})
	return jobId, nil
}

// readAPIIntoJob reads the workspace into the job's staged export, and moves
// the job on to be planned, or fails it saying why.
func readAPIIntoJob(ctx context.Context, jobId uuid.UUID, read APIRead) {
	opts := read.Options
	opts.Progress = func(ctx context.Context, r APIReadReport) error {
		noteAPIReadProgress(ctx, jobId, r)
		publishProgress(ctx, jobId, read.Workspace, importModels.StatusPending, apiReadStage, "")
		return nil
	}

	// The export streams into the store as it's written. Each side stops
	// the other: the one that failed first says why.
	var first firstError
	pr, pw := io.Pipe()
	staged := make(chan struct{})
	helpers.GoSafeNamed("business/SlackImport: stage the API read", func() {
		err := errors.New("staging the export stopped")
		defer func() {
			if err != nil {
				first.note(fmt.Errorf("saving the export: %w", err))
			}
			_ = pr.CloseWithError(err)
			close(staged)
		}()
		err = stageExport(ctx, StagedExportKey(jobId), pr)
	})
	report, readErr := ReadAPIExport(ctx, newAPIClient(read.Token), opts, pw)
	if readErr != nil {
		first.note(readErr)
	}
	_ = pw.CloseWithError(readErr)
	<-staged
	err := first.err()

	// What the read did is kept whether or not it was stopped. A job moves
	// on only from waiting: one discarded, or failed by the cleanup, while it
	// read stays as it is.
	after := context.WithoutCancel(ctx)
	noteAPIReadProgress(after, jobId, report)
	noteAPIReadNotes(after, jobId, report.Notes)
	if err != nil {
		msg := apiReadFailure(err, opts.MaxBytes)
		if moved, _ := importModels.UpdateStatusFrom(after, jobId, []string{importModels.StatusPending},
			importModels.StatusFailed, strPtr("failed"), &msg); moved {
			helpers.LogWarnWithContext(after, "SlackImport API read job=%s failed: %s", jobId, msg)
			publishProgress(after, jobId, read.Workspace, importModels.StatusFailed, "failed", msg)
		}
		return
	}
	moved, err := importModels.UpdateStatusFrom(after, jobId, []string{importModels.StatusPending},
		importModels.StatusValidating, strPtr("validating"), nil)
	if err != nil || !moved {
		if err := DeleteStagedZip(after, jobId); err != nil {
			helpers.LogWarnWithContext(after, "SlackImport API read job=%s: deleting the export of a job that moved on: %+v", jobId, err)
		}
		return
	}
	publishProgress(after, jobId, read.Workspace, importModels.StatusValidating, "validating", "")
}

// firstError keeps the first of the errors noted.
type firstError struct {
	mu    sync.Mutex
	first error
}

func (f *firstError) note(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.first == nil {
		f.first = err
	}
}

func (f *firstError) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.first
}

// noteAPIReadProgress keeps how far the read is on the job, as progress.read
// (its notes go to the job's errors).
func noteAPIReadProgress(ctx context.Context, jobId uuid.UUID, r APIReadReport) {
	r.Notes = nil
	patch, err := json.Marshal(map[string]APIReadReport{"read": r})
	if err == nil {
		err = importModels.UpdateProgress(ctx, jobId, patch)
	}
	if err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport API read job=%s: noting progress: %+v", jobId, err)
	}
}

// noteAPIReadNotes writes what the read left out to the job's errors list, as
// warnings, in one statement.
func noteAPIReadNotes(ctx context.Context, jobId uuid.UUID, notes []APIReadNote) {
	if len(notes) == 0 {
		return
	}
	entries := make([]importModels.ImportError, 0, len(notes))
	for _, n := range notes {
		entity := importModels.EntityChannel
		if n.Code == APINoteFile {
			entity = importModels.EntityFile
		}
		entries = append(entries, importModels.ImportError{
			EntityType: entity, SourceID: n.ID, Severity: importModels.SeverityWarning,
			Code: n.Code, Message: apiNoteWords(n),
		})
	}
	importModels.LogImportErrors(ctx, jobId, nil, entries)
}

// apiNoteWords is what a note says, for the job's errors list. Pure.
func apiNoteWords(n APIReadNote) string {
	what := n.Name
	if what == "" {
		what = n.ID
	}
	switch n.Code {
	case APINoteKind:
		return fmt.Sprintf("Slack didn't give the token its %s conversations (%s).", kindWords(n.ID), n.Reason)
	case APINoteConversation:
		return fmt.Sprintf("Slack didn't give the token %s (%s).", what, n.Reason)
	}
	switch n.Reason {
	case "external":
		return what + " is shared from another service, so it stays there."
	case "gone":
		return what + " is no longer in Slack."
	case "too_large":
		return what + " is larger than the file limit."
	case "over_limit":
		return what + " would take the import over this server's limit."
	case "unreadable":
		return "Slack wouldn't give the token " + what + "."
	}
	return fmt.Sprintf("%s couldn't be read (%s).", what, n.Reason)
}

// kindWords is a kind of conversation as people say it. Pure.
func kindWords(kind string) string {
	switch kind {
	case "public_channel":
		return "public channel"
	case "private_channel":
		return "private channel"
	case "mpim":
		return "group DM"
	case "im":
		return "DM"
	}
	return kind
}

// apiReadFailure is why a read failed, for the admin, never with the token
// (it is in no address or error) or an address's query. Pure.
func apiReadFailure(err error, maxBytes int64) string {
	var se *slackapi.Error
	switch {
	case errors.Is(err, ErrAPIReadTooLarge):
		return fmt.Sprintf("This workspace's history is larger than this server takes (%s). "+
			"Whoever runs it can raise the limit (EXPORT_MAX_BYTES).", helpers.ReadableBytes(maxBytes))
	case errors.As(err, &se) && (se.Code == "invalid_auth" || se.Code == "not_authed" || se.Code == "token_revoked" ||
		se.Code == "token_expired" || se.Code == "account_inactive"):
		return "Slack didn't accept the token (" + se.Code + "). Check it, and start the read again."
	case errors.As(err, &se) && se.Code == "missing_scope":
		return "The token is missing a permission the read needs (" + se.Needed + "). Add it to the Slack app, and start the read again."
	case errors.As(err, &se) && se.Code == "ratelimited":
		return "Slack kept asking the read to slow down. Start it again later."
	}
	return "Reading from Slack failed: " + helpers.WithoutURLQueries(err.Error())
}
