package domain

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/UserProjectNotification"
	"github.com/google/uuid"
)

func CreateProjectNotificationType(ctx context.Context, userId string, projectId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		INSERT INTO users_project_notification 
		(user_id, project_id, notification_type, updated_at)
		VALUES ($1, $2, $3, $4)
	`

	err = models.CreatePojectNotificationType(query, userId, projectId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectNotificationType Failed to create user's project notification type err: %+v",
			err)
		return
	}
	return
}

func UpdateProjectNotificationType(ctx context.Context, userId string, projectId string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		UPDATE users_project_notification
        SET notification_type = $1, updated_at = $2
		WHERE user_id = $3 AND project_id = $4
	`

	err = models.UpdateProjectNotificationType(query, userId, projectId, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectNotificationType Failed to update user's project notification type err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormProject(ctx context.Context, userId string, projectId string) (err error) {
	query := `
		DELETE FROM users_project_notification
		WHERE user_id = $1 AND project_id = $2
	`

	err = models.DeleteNotificationTypeWhenUserIsRemovedFormProject(query, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteNotificationTypeWhenUserIsRemovedFormProject Failed to delete user's project notification type err: %+v",
			err)
		return
	}
	return
}

func GetNotificationTypeByUserIdAndProjectId(ctx context.Context, userId string, projectId string) (notificationType string, err error) {
	query := `
		SELECT notification_type
		FROM users_project_notification
		WHERE user_id = $1 AND project_id = $2
	`

	notificationType, err = models.GetNotificationTypeByUserIdAndProjectId(query, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetNotificationTypeByUserIdAndProjectId Failed to user's notification err: %+v",
			err)
		return
	}
	return

}

// LevelsOnTask is the setting, for the project the task taskID is in, of each
// of userIDs who has one, by lower-case user id: one read for everyone an
// event about the task would tell. Someone with no setting has no entry and
// hears (notifylevel.Hears), and a setting is no proof of being in the
// project: who may read the task is asked apart.
func LevelsOnTask(ctx context.Context, taskID string, userIDs []string) (map[string]string, error) {
	asked := make([]OnTask, len(userIDs))
	for i, u := range userIDs {
		asked[i] = OnTask{User: u, Task: taskID}
	}
	standing, err := StandingsOnTasks(ctx, asked)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LevelsOnTask could not read the settings on task %s err: %+v", taskID, err)
		return nil, err
	}
	return setLevels(userIDs, func(i int) (string, bool) { return standing[i].Level, standing[i].Set }), nil
}

// setLevels is the level of each of userIDs who has a setting (level(i), by
// index), by lower-case user id.
func setLevels(userIDs []string, level func(i int) (string, bool)) map[string]string {
	levels := make(map[string]string, len(userIDs))
	for i, u := range userIDs {
		if l, set := level(i); set {
			levels[strings.ToLower(u)] = l
		}
	}
	return levels
}

// OnTask is one person (a user id) asked about a task (its uuid), or about a
// Reply (a comment's uuid) on it.
type OnTask struct {
	User, Task, Reply string
}

// TaskStanding is where an OnTask person stands now: their setting for the
// task's project (Set false with none: they hear, notifylevel.HearsAt), and
// whether the Reply asked of is still there (true with none asked). Tasks
// keep no read marker, so nobody is read as caught up on one. A pair that
// isn't ids stands nowhere: no setting, and no reply.
type TaskStanding struct {
	Level          string
	Set, ReplyLive bool
}

// StandingsOnTasks is each of asked's TaskStanding, by index, in one read.
func StandingsOnTasks(ctx context.Context, asked []OnTask) ([]TaskStanding, error) {
	out := make([]TaskStanding, len(asked))
	var users, tasks, replies []string
	var at []int
	for i, a := range asked {
		if !isID(a.User) || !isID(a.Task) || (a.Reply != "" && !isID(a.Reply)) {
			continue
		}
		users, tasks, replies, at = append(users, a.User), append(tasks, a.Task), append(replies, a.Reply), append(at, i)
	}
	if len(at) == 0 {
		return out, nil
	}
	type row struct {
		n     int
		level *string
		live  bool
	}
	rows, err := pgexec.Query(ctx, `
		SELECT a.n, s.notification_type::text, a.c = '' OR (c.id IS NOT NULL AND c.deleted_at IS NULL)
		FROM unnest($1::uuid[], $2::uuid[], $3::text[]) WITH ORDINALITY AS a(u, t, c, n)
		LEFT JOIN tasks tk ON tk.id = a.t
		LEFT JOIN users_project_notification s ON s.project_id = tk.project_id AND s.user_id = a.u
		LEFT JOIN comments c ON c.id = NULLIF(a.c, '')::uuid`,
		func(r pgexec.Row) (x row, err error) {
			err = r.Scan(&x.n, &x.level, &x.live)
			return x, err
		}, pgexec.Array(users), pgexec.Array(tasks), pgexec.Array(replies))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[at[r.n-1]] = TaskStanding{Level: helpers.Deref(r.level), Set: r.level != nil, ReplyLive: r.live}
	}
	return out, nil
}

// isID is whether s is a uuid, as Postgres reads one.
func isID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// InProject is one person (a user id) asked about a project (its uuid), or
// about an Update (a project update's id) in it.
type InProject struct {
	User, Project, Update string
}

// ProjectStanding is where an InProject person stands now: their setting for
// the project (Set false with none: they hear, notifylevel.HearsAt), and
// whether the project is there (not archived) with the Update asked of still
// in it (Live; the project alone with none asked). A setting is no proof of
// being in the project: who is in it is asked apart. A pair that isn't ids
// stands nowhere: no setting, and nothing live.
type ProjectStanding struct {
	Level     string
	Set, Live bool
}

// StandingsInProjects is each of asked's ProjectStanding, by index, in one
// read.
func StandingsInProjects(ctx context.Context, asked []InProject) ([]ProjectStanding, error) {
	out := make([]ProjectStanding, len(asked))
	var users, projects, updates []string
	var at []int
	for i, a := range asked {
		if !isID(a.User) || !isID(a.Project) || (a.Update != "" && !isID(a.Update)) {
			continue
		}
		users, projects, updates, at = append(users, a.User), append(projects, a.Project), append(updates, a.Update), append(at, i)
	}
	if len(at) == 0 {
		return out, nil
	}
	type row struct {
		n     int
		level *string
		live  bool
	}
	rows, err := pgexec.Query(ctx, `
		SELECT a.n, s.notification_type::text,
			p.id IS NOT NULL AND p.deleted_at IS NULL
			AND (a.d = '' OR (d.id IS NOT NULL AND d.deleted_at IS NULL AND d.project_uuid = a.p))
		FROM unnest($1::uuid[], $2::uuid[], $3::text[]) WITH ORDINALITY AS a(u, p, d, n)
		LEFT JOIN projects p ON p.id = a.p
		LEFT JOIN users_project_notification s ON s.project_id = a.p AND s.user_id = a.u
		LEFT JOIN project_updates d ON d.id = NULLIF(a.d, '')::uuid`,
		func(r pgexec.Row) (x row, err error) {
			err = r.Scan(&x.n, &x.level, &x.live)
			return x, err
		}, pgexec.Array(users), pgexec.Array(projects), pgexec.Array(updates))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[at[r.n-1]] = ProjectStanding{Level: helpers.Deref(r.level), Set: r.level != nil, Live: r.live}
	}
	return out, nil
}

// LevelsInProject is the setting for the project projectID of each of userIDs
// who has one (StandingsInProjects), or of everyone who has one when userIDs
// is nil, by lower-case user id: one read for everyone an event in the
// project would tell. Someone with no setting has no entry and hears
// (notifylevel.Hears), and a setting is no proof of being in the project.
func LevelsInProject(ctx context.Context, projectID string, userIDs []string) (map[string]string, error) {
	if userIDs != nil {
		asked := make([]InProject, len(userIDs))
		for i, u := range userIDs {
			asked[i] = InProject{User: u, Project: projectID}
		}
		standing, err := StandingsInProjects(ctx, asked)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/LevelsInProject could not read the settings in project %s err: %+v", projectID, err)
			return nil, err
		}
		return setLevels(userIDs, func(i int) (string, bool) { return standing[i].Level, standing[i].Set }), nil
	}
	rows, err := pgexec.Query(ctx, `
		SELECT lower(user_id::text), notification_type::text
		FROM users_project_notification
		WHERE project_id = $1`, scanLevel, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LevelsInProject could not read the settings in project %s err: %+v", projectID, err)
		return nil, err
	}
	return byUser(rows), nil
}

// userLevel is one person's setting, as the level reads scan it. The reads
// run through pgexec.Query, whose type is inferred, so the store-loop guard
// (helpers) sees them from any loop that reaches them; it can't see a call
// with explicit type arguments (pgexec.Pairs[string, string]).
type userLevel struct{ user, level string }

func scanLevel(r pgexec.Row) (userLevel, error) {
	var l userLevel
	err := r.Scan(&l.user, &l.level)
	return l, err
}

// byUser is the levels by user id.
func byUser(rows []userLevel) map[string]string {
	levels := make(map[string]string, len(rows))
	for _, r := range rows {
		levels[r.user] = r.level
	}
	return levels
}
