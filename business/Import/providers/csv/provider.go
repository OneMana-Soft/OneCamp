package csv

import (
	"context"
	"errors"
	"io"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// Provider brings an uploaded CSV of tasks in as one project.
type Provider struct{}

func init() {
	importProvider.Register(&Provider{})
}

// Name is the registry key.
func (p *Provider) Name() string { return importModels.ProviderCSV }

// Capabilities: one project, its tasks and sub-tasks. A CSV has no comments
// or files, and its project goes under the import's own team.
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTeams | importProvider.CapProjects | importProvider.CapTasks | importProvider.CapSubtasks
}

// SupportedSources: an uploaded file.
func (p *Provider) SupportedSources() []string {
	return []string{importModels.SourceBackupCSV}
}

// tables are the files jobs uploaded, read; sheets are what each says, read
// with the admin's last choices (Plan reads it again with new ones).
var (
	tables = importProvider.NewJobCache[*table]()
	sheets = importProvider.NewJobCache[*sheet]()
)

// Validate reads the file: a CSV whose columns name the tasks' titles, with
// a task under them, and no more than an import brings.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions) error {
	t, err := loadTable(ctx, j)
	if err != nil {
		return err
	}
	return check(t)
}

func check(t *table) error {
	for _, c := range t.cols {
		if c.prop == propTitle {
			if len(t.rows) == 0 {
				return errNoRows
			}
			return nil
		}
	}
	return errNoTitle
}

// Plan reads the file with the admin's choices and says what comes in: the
// counts, the statuses and priorities to map, each column as it is read, and
// the people the file names.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	t, err := loadTable(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	if err := check(t); err != nil {
		return nil, nil, err
	}
	s := read(t, j.SourceWorkspaceName, opts)
	sheets.Store(j.Id, s)
	return s.plan, nil, nil
}

// IterUsers streams the people the file names.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	return stream(ctx, j, opts, func(s *sheet) []importProvider.SourceUser { return s.people })
}

// IterTeams streams none: the project goes under the import's own team.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	return stream(ctx, j, opts, func(*sheet) []importProvider.SourceTeam { return nil })
}

// IterProjects streams the one project, named as the upload was.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	return stream(ctx, j, opts, func(s *sheet) []importProvider.SourceProject {
		return []importProvider.SourceProject{s.project}
	})
}

// IterTasksOfProject streams the top-level tasks, then a marker for each
// task with sub-tasks.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, _ string) (<-chan importProvider.SourceTask, <-chan error) {
	return stream(ctx, j, opts, func(s *sheet) []importProvider.SourceTask { return s.tasks })
}

// IterSubtasksOfTask streams the rows whose parent is the task.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	return stream(ctx, j, opts, func(s *sheet) []importProvider.SourceTask { return s.children[taskSourceId] })
}

// IterCommentsOfTask streams none: a CSV has no comments.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, _ string) (<-chan importProvider.SourceComment, <-chan error) {
	return stream(ctx, j, opts, func(*sheet) []importProvider.SourceComment { return nil })
}

// errNoFiles: a CSV names no files, so no stage asks for one.
var errNoFiles = errors.New("a CSV import has no files")

// FetchAttachment has nothing to fetch.
func (p *Provider) FetchAttachment(context.Context, *importModels.Job, importProvider.JobOptions, importProvider.SourceAttachment, io.Writer) (string, int64, error) {
	return "", 0, errNoFiles
}

// DefaultStatusMap is empty: OneCamp's own status names and the rivals'
// common ones are what importProvider.StatusFromHeuristic reads.
func (p *Provider) DefaultStatusMap() map[string]string { return map[string]string{} }

// DefaultPriorityMap is empty: priorities are read by their words
// (taskPriority.FromWords).
func (p *Provider) DefaultPriorityMap() map[string]string { return map[string]string{} }

// CleanupJob forgets a finished job's file (importProvider.JobCleaner).
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	tables.Evict(id)
	sheets.Evict(id)
}

// loadTable is the job's file, read once.
func loadTable(ctx context.Context, j *importModels.Job) (*table, error) {
	if t := tables.Load(j.Id); t != nil {
		return t, nil
	}
	if j.RawObjectKey == nil || *j.RawObjectKey == "" {
		return nil, errors.New("The import has no file. Upload the CSV again.")
	}
	f, err := importProvider.OpenStagedFile(ctx, *j.RawObjectKey)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t, err := readTable(f, maxBytes)
	if err != nil {
		return nil, err
	}
	tables.Store(j.Id, t)
	return t, nil
}

// loadSheet is what the job's file says with the admin's choices: Plan's, or
// read again in a process that didn't plan it.
func loadSheet(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*sheet, error) {
	if s := sheets.Load(j.Id); s != nil {
		return s, nil
	}
	t, err := loadTable(ctx, j)
	if err != nil {
		return nil, err
	}
	s := read(t, j.SourceWorkspaceName, opts)
	sheets.Store(j.Id, s)
	return s, nil
}

// stream sends what of the job's sheet pick gives, as the orchestrator
// reads a provider: an item channel and an error channel, both closed when
// done.
func stream[T any](ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, pick func(*sheet) []T) (<-chan T, <-chan error) {
	out := make(chan T, 32)
	errCh := make(chan error, 1)
	helpers.GoSafeNamed("business/Import/providers/csv/provider:stream", func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("csv.stream", errCh)
		s, err := loadSheet(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, v := range pick(s) {
			select {
			case <-ctx.Done():
				return
			case out <- v:
			}
		}
	})
	return out, errCh
}
