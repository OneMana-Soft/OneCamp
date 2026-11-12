// Package models (Release) keeps a project's releases (migration 580): the
// releases, the tasks in each and the stages builds and deploys report. Every
// read takes live releases and live release tasks only
// (models/postgres/SoftDelete.Live); a deleted release keeps its tasks and
// stages for its restore, and a task taken out keeps its row for its put-back.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The states a release is in.
const (
	Planned  = "planned"
	Released = "released"
)

// Sources of a release's task.
const (
	Manual = "manual" // a person put it in
	Git    = "git"    // a tag's commits named it (P10-1b job 2)
)

// ErrTaken is a name another live release of the project has.
var ErrTaken = errors.New("a live release of the project has that name")

// ErrFull is a release that would hold more than its most tasks.
var ErrFull = errors.New("the release holds its most tasks")

// Release is one release's row.
type Release struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	// TargetDate is the day it should go out ("2006-01-02"), or nil.
	TargetDate *string    `json:"target_date"`
	ReleasedAt *time.Time `json:"released_at"`
	// Repo, Tag and TagSHA are the repository tag it went out as; "" for a
	// release made by hand that no tag has shipped.
	Repo      string     `json:"repo"`
	Tag       string     `json:"tag"`
	TagSHA    string     `json:"tag_sha"`
	Notes     string     `json:"notes"`
	CreatedBy *uuid.UUID `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Member is a task in a release, and how it came to be there.
type Member struct {
	Task    uuid.UUID
	Source  string
	AddedAt time.Time
	// LeftUnfinished is a task the release left still to do when it went
	// out, which Mark released moved on (BurndownMembersOf's answer only).
	LeftUnfinished bool
}

// Stage is what a repository's build or deploy last said about a release.
type Stage struct {
	Provider   string    `json:"provider"`
	Repo       string    `json:"repo"`
	Kind       string    `json:"kind"` // build, deploy
	Name       string    `json:"name"` // the workflow, or the environment
	Status     string    `json:"status"`
	SHA        string    `json:"sha"`
	URL        string    `json:"url"`
	ReportedAt time.Time `json:"reported_at"`
}

const columns = `id, project_uuid, name, state, to_char(target_date, 'YYYY-MM-DD'), released_at,
	COALESCE(repo, ''), COALESCE(tag, ''), COALESCE(tag_sha, ''), notes, created_by, created_at, updated_at`

func scan(r pgexec.Row) (Release, error) {
	var x Release
	var by uuid.NullUUID
	err := r.Scan(&x.ID, &x.ProjectID, &x.Name, &x.State, &x.TargetDate, &x.ReleasedAt,
		&x.Repo, &x.Tag, &x.TagSHA, &x.Notes, &by, &x.CreatedAt, &x.UpdatedAt)
	if by.Valid {
		x.CreatedBy = &by.UUID
	}
	return x, err
}

// keys is List's order over the release a: planned before out and the
// soonest target day first, none last (both rising); then the latest out
// first, never out last, and the newest made first (both falling); then the
// id, which no two releases share (rising). None is NULL, so a page's cursor
// compares with each.
func keys(a string) (rising, falling [2]string, id string) {
	return [2]string{a + ".state = 'released'", "COALESCE(" + a + ".target_date, 'infinity')"},
		[2]string{"COALESCE(" + a + ".released_at, '-infinity')", a + ".created_at"}, a + ".id"
}

// listSQL reads a page of a project's live releases ($1), limit ($3) of
// them in keys' order, after the release $2 (NULL for the first page).
var listSQL = func() string {
	up, down, id := keys("r")
	cUp, cDown, cID := keys("c")
	row := func(k [2]string) string { return "(" + k[0] + ", " + k[1] + ")" }
	after := row(up) + " > " + row(cUp) + " OR (" + row(up) + " = " + row(cUp) + " AND (" +
		row(down) + " < " + row(cDown) + " OR (" + row(down) + " = " + row(cDown) + " AND " + id + " > " + cID + ")))"
	return `SELECT ` + columns + ` FROM releases r WHERE r.project_uuid = $1 AND ` + softDelete.Live("r") + `
		AND ($2::uuid IS NULL OR EXISTS (SELECT 1 FROM releases c WHERE c.id = $2 AND c.project_uuid = $1 AND (` + after + `)))
		ORDER BY ` + up[0] + `, ` + up[1] + `, ` + down[0] + ` DESC, ` + down[1] + ` DESC, ` + id + `
		LIMIT $3`
}()

// List is a page of a project's live releases, limit of them: those still
// planned first, soonest target first (none last), then those out, latest
// first, and the newest made first between those alike. after is the last
// release of the page before (uuid.Nil for the first page); one that isn't
// the project's reads an empty page.
func List(ctx context.Context, project, after uuid.UUID, limit int) ([]Release, error) {
	return pgexec.Query(ctx, listSQL, scan, project, uuid.NullUUID{UUID: after, Valid: after != uuid.Nil}, limit)
}

// Get is one live release, and whether there is one.
func Get(ctx context.Context, id uuid.UUID) (Release, bool, error) {
	return pgexec.QueryOne(ctx, `SELECT `+columns+` FROM releases WHERE id = $1 AND `+softDelete.Live(""), scan, id)
}

// Create makes a planned release. ErrTaken: a live release of the project has
// the name.
func Create(ctx context.Context, project uuid.UUID, name string, target *string, notes string, by uuid.UUID) (Release, error) {
	x, _, err := pgexec.QueryOne(ctx, `INSERT INTO releases (project_uuid, name, target_date, notes, created_by)
		VALUES ($1, $2, $3::date, $4, $5) RETURNING `+columns, scan, project, name, target, notes, softDelete.By(by))
	if helpers.IsUniqueViolation(err) {
		return Release{}, ErrTaken
	}
	return x, err
}

// Update sets a live release's name, target day and notes, and answers
// whether it was there. ErrTaken: a live release of the project has the name.
func Update(ctx context.Context, id uuid.UUID, name string, target *string, notes string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE releases SET name = $2, target_date = $3::date, notes = $4, updated_at = NOW()
		WHERE id = $1 AND `+softDelete.Live(""), id, name, target, notes)
	if helpers.IsUniqueViolation(err) {
		return false, ErrTaken
	}
	return n > 0, err
}

// Ship marks a live planned release out now, and answers whether it moved:
// one already out keeps when it went.
func Ship(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE releases SET state = 'released', released_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND state = 'planned' AND `+softDelete.Live(""), id)
	return n > 0, err
}

// Delete deletes a live release softly, by whom, and answers whether it was
// live. Its tasks and stages stay for its restore.
func Delete(ctx context.Context, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.Release, id, by, "")
}

// Restore brings a deleted release back with its tasks and stages, and
// answers whether it was deleted. ErrTaken: a live release has its name.
func Restore(ctx context.Context, id uuid.UUID) (bool, error) {
	back, err := softDelete.Restore(ctx, softDelete.Release, id)
	if errors.Is(err, softDelete.ErrTaken) {
		return false, ErrTaken
	}
	return back, err
}

// MembersOf is each of the releases' live tasks, in the order they joined, in
// one read however many releases: a task taken out is in none of them.
func MembersOf(ctx context.Context, releases []uuid.UUID) (map[uuid.UUID][]Member, error) {
	out := make(map[uuid.UUID][]Member, len(releases))
	if len(releases) == 0 {
		return out, nil
	}
	type row struct {
		release uuid.UUID
		m       Member
	}
	rows, err := pgexec.Query(ctx, `SELECT release_id, task_uuid, source, added_at FROM release_tasks
		WHERE release_id = ANY($1::uuid[]) AND `+softDelete.Live("")+` ORDER BY added_at, task_uuid`, func(r pgexec.Row) (row, error) {
		var x row
		err := r.Scan(&x.release, &x.m.Task, &x.m.Source, &x.m.AddedAt)
		return x, err
	}, pq.Array(releases))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.release] = append(out[x.release], x.m)
	}
	return out, nil
}

// BurndownMembersOf is a release's tasks for its burndown, in the order they
// joined: its live ones, and those it left unfinished when it went out. Mark
// released takes those out in the transaction that marks it out
// (ShipMoving), so their removal is stamped with the very moment it went out
// (NOW() is the transaction's); a task taken out by hand since isn't one.
func BurndownMembersOf(ctx context.Context, release uuid.UUID) ([]Member, error) {
	return pgexec.Query(ctx, `SELECT t.task_uuid, t.source, t.added_at, t.deleted_at IS NOT NULL FROM release_tasks t
		JOIN releases r ON r.id = t.release_id
		WHERE t.release_id = $1 AND (`+softDelete.Live("t")+` OR t.deleted_at = r.released_at)
		ORDER BY t.added_at, t.task_uuid`, func(r pgexec.Row) (Member, error) {
		var m Member
		err := r.Scan(&m.Task, &m.Source, &m.AddedAt, &m.LeftUnfinished)
		return m, err
	}, release)
}

// AddTasks puts tasks in a live release, from source, by whom, in one write
// however many, and answers the ids it added (a task already there stays as
// it joined; one taken out comes back in its own row, as from source, by
// whom, now). Under the release row's lock, so two adds at once can't pass
// most between them: past most live tasks nothing is added (ErrFull). A
// release no longer live (deleted since its caller read it) is
// helpers.ErrNotFound, and nothing is added.
func AddTasks(ctx context.Context, release uuid.UUID, tasks []uuid.UUID, source string, by uuid.UUID, most int) ([]string, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	var added []string
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		// The lock is the live release's row's: a second add waits for this
		// one, and a delete either waits too or went first, when the
		// release isn't live.
		_, live, err := pgexec.QueryOne(ctx, `SELECT id::text FROM releases WHERE id = $1 AND `+softDelete.Live("")+` FOR NO KEY UPDATE`, pgexec.Text, release)
		if err == nil && !live {
			err = helpers.ErrNotFound
		}
		if err != nil {
			return err
		}
		added, err = pgexec.Query(ctx, `INSERT INTO release_tasks (release_id, task_uuid, source, added_by)
			SELECT r.id, t, $3, $4 FROM releases r, (SELECT DISTINCT unnest($2::uuid[]) AS t) named
			WHERE r.id = $1 AND `+softDelete.Live("r")+`
			ON CONFLICT (release_id, task_uuid) DO UPDATE SET source = EXCLUDED.source, added_by = EXCLUDED.added_by,
				added_at = NOW(), deleted_at = NULL, deleted_by = NULL
			WHERE release_tasks.deleted_at IS NOT NULL
			RETURNING task_uuid::text`, pgexec.Text, release, pq.Array(tasks), source, softDelete.By(by))
		if err != nil || len(added) == 0 {
			return err
		}
		n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM release_tasks WHERE release_id = $1 AND `+softDelete.Live(""), release)
		if err == nil && n > most {
			err = ErrFull
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// RemoveTask takes a task out of a release, by whom, and answers whether it
// was in it. The delete is soft: the row stays, marked, for a put-back.
func RemoveTask(ctx context.Context, release, task, by uuid.UUID) (bool, error) {
	n, err := removeTasks(ctx, release, []uuid.UUID{task}, by)
	return n > 0, err
}

// removeTasks takes tasks out of a release, softly, by whom, in one write,
// and answers how many were in it.
func removeTasks(ctx context.Context, release uuid.UUID, tasks []uuid.UUID, by uuid.UUID) (int64, error) {
	return pgexec.Exec(ctx, `UPDATE release_tasks SET deleted_at = NOW(), deleted_by = $3
		WHERE release_id = $1 AND task_uuid = ANY($2::uuid[]) AND `+softDelete.Live(""), release, pq.Array(tasks), softDelete.By(by))
}

// ShipMoving marks a live planned release out now, as Ship does, and in the
// same transaction takes tasks out of it, softly, at the very moment it went
// out (so BurndownMembersOf reads them as left unfinished), putting them in
// release to, when there is one, as AddTasks does (past most, ErrFull; to no
// longer live, helpers.ErrNotFound). Answers how many it took out: none when
// the release was out already, which keeps when it went and its tasks. On
// any error nothing changes.
func ShipMoving(ctx context.Context, id uuid.UUID, tasks []uuid.UUID, to *uuid.UUID, by uuid.UUID, most int) (int, error) {
	moved := 0
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		shipped, err := Ship(ctx, id)
		if err != nil || !shipped || len(tasks) == 0 {
			return err
		}
		n, err := removeTasks(ctx, id, tasks, by)
		if err != nil {
			return err
		}
		moved = int(n)
		if to != nil {
			_, err = AddTasks(ctx, *to, tasks, Manual, by, most)
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return moved, nil
}

// GetMany is those of releases that are live, in one read however many.
func GetMany(ctx context.Context, releases []uuid.UUID) ([]Release, error) {
	if len(releases) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT `+columns+` FROM releases WHERE id = ANY($1::uuid[]) AND `+softDelete.Live(""), scan, pq.Array(releases))
}

// OfTask is the live releases of project task is in, newest made first.
func OfTask(ctx context.Context, project, task uuid.UUID) ([]Release, error) {
	return pgexec.Query(ctx, `SELECT `+columns+` FROM releases WHERE project_uuid = $1 AND `+softDelete.Live("")+`
		AND id IN (SELECT release_id FROM release_tasks WHERE task_uuid = $2 AND `+softDelete.Live("")+`)
		ORDER BY created_at DESC, id DESC`, scan, project, task)
}

// SetTask puts task in releases add and takes it out of releases remove, by
// whom, in one transaction: each add as AddTasks puts one in (past most live
// tasks, ErrFull; a release no longer live, helpers.ErrNotFound), each
// remove as RemoveTask takes one out, softly. On any error nothing changes.
func SetTask(ctx context.Context, task uuid.UUID, add, remove []uuid.UUID, by uuid.UUID, most int) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		for _, r := range add {
			if _, err := AddTasks(ctx, r, []uuid.UUID{task}, Manual, by, most); err != nil {
				return err
			}
		}
		for _, r := range remove {
			if _, err := RemoveTask(ctx, r, task, by); err != nil {
				return err
			}
		}
		return nil
	})
}

// TasksFiltered is what a task list filter on project's releases reads, in
// one statement: without none, the tasks in any of releases; with none, the
// tasks in some live release of project and in none of releases (those a
// "No release" filter leaves out). Releases not live, or not project's,
// hold none; nil releases is none chosen (pq sends it as NULL).
func TasksFiltered(ctx context.Context, project uuid.UUID, releases []uuid.UUID, none bool) ([]string, error) {
	return pgexec.Query(ctx, `SELECT t.task_uuid::text FROM release_tasks t JOIN releases r ON r.id = t.release_id
		WHERE r.project_uuid = $1 AND `+softDelete.Live("r")+` AND `+softDelete.Live("t")+`
		GROUP BY t.task_uuid HAVING COALESCE(bool_or(r.id = ANY($2::uuid[])), false) <> $3 ORDER BY t.task_uuid`, pgexec.Text, project, pq.Array(releases), none)
}

// StagesOf is each of the releases' stages, builds then deploys, by name, in
// one read however many releases.
func StagesOf(ctx context.Context, releases []uuid.UUID) (map[uuid.UUID][]Stage, error) {
	out := make(map[uuid.UUID][]Stage, len(releases))
	if len(releases) == 0 {
		return out, nil
	}
	type row struct {
		release uuid.UUID
		s       Stage
	}
	rows, err := pgexec.Query(ctx, `SELECT release_id, provider, repo, kind, name, status, sha, url, reported_at FROM release_stages
		WHERE release_id = ANY($1::uuid[]) ORDER BY kind, name, repo`, func(r pgexec.Row) (row, error) {
		var x row
		err := r.Scan(&x.release, &x.s.Provider, &x.s.Repo, &x.s.Kind, &x.s.Name, &x.s.Status, &x.s.SHA, &x.s.URL, &x.s.ReportedAt)
		return x, err
	}, pq.Array(releases))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.release] = append(out[x.release], x.s)
	}
	return out, nil
}
