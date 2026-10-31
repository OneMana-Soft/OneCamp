package provider

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// None is a stream with nothing in it, for what a provider brings none of:
// a base of tables, Airtable's or a spreadsheet, brings no people, teams,
// projects, tasks or comments.
func None[T any]() (<-chan T, <-chan error) {
	c, e := make(chan T), make(chan error)
	close(c)
	close(e)
	return c, e
}

// NoTasks is the task-shaped half of a provider whose source has no tasks
// (Airtable's bases, Google's spreadsheets, Miro's boards): no people, teams,
// projects, tasks, subtasks or comments to stream, and no statuses or
// priorities to map. Such a provider embeds it.
type NoTasks struct{}

func (NoTasks) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (NoTasks) DefaultPriorityMap() map[string]string { return map[string]string{} }

func (NoTasks) IterUsers(context.Context, *importModels.Job, JobOptions) (<-chan SourceUser, <-chan error) {
	return None[SourceUser]()
}
func (NoTasks) IterTeams(context.Context, *importModels.Job, JobOptions) (<-chan SourceTeam, <-chan error) {
	return None[SourceTeam]()
}
func (NoTasks) IterProjects(context.Context, *importModels.Job, JobOptions) (<-chan SourceProject, <-chan error) {
	return None[SourceProject]()
}
func (NoTasks) IterTasksOfProject(context.Context, *importModels.Job, JobOptions, string) (<-chan SourceTask, <-chan error) {
	return None[SourceTask]()
}
func (NoTasks) IterSubtasksOfTask(context.Context, *importModels.Job, JobOptions, string) (<-chan SourceTask, <-chan error) {
	return None[SourceTask]()
}
func (NoTasks) IterCommentsOfTask(context.Context, *importModels.Job, JobOptions, string) (<-chan SourceComment, <-chan error) {
	return None[SourceComment]()
}

// Stream streams, on the channels an Iter* method returns, what load gives
// back. It stops when ctx is done and turns a panic into an error, as every
// iterator must.
func Stream[S any](ctx context.Context, name string, load func() ([]S, error)) (<-chan S, <-chan error) {
	out := make(chan S, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr(name, errCh)
		items, err := load()
		if err != nil {
			errCh <- err
			return
		}
		for _, it := range items {
			select {
			case <-ctx.Done():
				return
			case out <- it:
			}
		}
	}()
	return out, errCh
}

// StreamTasks streams the items load gives back that match, each converted
// to a SourceTask. For providers that read their source once into a snapshot.
func StreamTasks[T any](ctx context.Context, name string, load func() ([]T, error), match func(T) bool, convert func(T) SourceTask) (<-chan SourceTask, <-chan error) {
	return Stream(ctx, name, func() ([]SourceTask, error) {
		items, err := load()
		if err != nil {
			return nil, err
		}
		var out []SourceTask
		for _, it := range items {
			if match(it) {
				out = append(out, convert(it))
			}
		}
		return out, nil
	})
}
