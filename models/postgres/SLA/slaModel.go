// Package models (SLA) keeps SLA timers (migration 596, linear-24): a
// project's rules (sla_rules, soft delete), each task's one timer
// (task_slas, never deleted) and the project's work week and zone (two
// columns of project_settings, 593's one row per project).
package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Rule is one of a project's SLA rules. A list left empty matches anything.
type Rule struct {
	ID            uuid.UUID   `json:"id"`
	ProjectID     uuid.UUID   `json:"project_uuid"`
	Name          string      `json:"name"`
	Priorities    []string    `json:"priorities"`
	Labels        []string    `json:"labels"`
	TypeIDs       []uuid.UUID `json:"type_ids"`
	Sources       []string    `json:"sources"`
	TargetMinutes int         `json:"target_minutes"`
	Clock         string      `json:"clock"`
	Starts        string      `json:"starts"`
	Rank          int         `json:"rank"`
	// Virtual is a default rule shown while the project never saved one.
	Virtual   bool       `json:"virtual,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// live is false for a removed rule (deleted_at set).
	live bool
}

// Timer is one task's timer as kept.
type Timer struct {
	TaskID       uuid.UUID
	ProjectID    uuid.UUID
	RuleID       *uuid.UUID
	Source       string
	StartedAt    time.Time
	DueAt        *time.Time
	RiskAt       *time.Time
	State        string
	MetAt        *time.Time
	WarnedAt     *time.Time
	BreachToldAt *time.Time
	EndedAt      *time.Time
	EndedReason  string
}

// The timer states as kept.
const (
	Running = "running"
	Met     = "met"
	Missed  = "missed"
	Ended   = "ended"
)

const ruleColumns = `id, project_uuid, name, priorities, labels, type_ids, sources, target_minutes, clock, starts, rank, updated_at, deleted_at IS NULL`

func scanRule(r pgexec.Row) (Rule, error) {
	var x Rule
	var types []string
	var at time.Time
	err := r.Scan(&x.ID, &x.ProjectID, &x.Name, pq.Array(&x.Priorities), pq.Array(&x.Labels), pq.Array(&types),
		pq.Array(&x.Sources), &x.TargetMinutes, &x.Clock, &x.Starts, &x.Rank, &at, &x.live)
	for _, s := range types {
		if id, perr := uuid.Parse(s); perr == nil {
			x.TypeIDs = append(x.TypeIDs, id)
		}
	}
	x.UpdatedAt = &at
	return x, err
}

// Rules is the project's live rules in rank order, and whether it ever saved
// one (a removed one counts): one read.
func Rules(ctx context.Context, project uuid.UUID) ([]Rule, bool, error) {
	rows, err := pgexec.Query(ctx, `SELECT `+ruleColumns+` FROM sla_rules WHERE project_uuid = $1
		ORDER BY rank, created_at, id`, scanRule, project)
	if err != nil {
		return nil, false, err
	}
	var live []Rule
	for _, r := range rows {
		if r.live {
			live = append(live, r)
		}
	}
	return live, len(rows) > 0, nil
}

// SaveRule writes r, new (made) or over the live rule with its id; found is
// false for an id the project has no live rule by.
func SaveRule(ctx context.Context, r Rule, by uuid.UUID, made bool) (bool, error) {
	types := make([]string, 0, len(r.TypeIDs))
	for _, id := range r.TypeIDs {
		types = append(types, id.String())
	}
	args := []any{r.ID, r.ProjectID, r.Name, pq.Array(nonNil(r.Priorities)), pq.Array(nonNil(r.Labels)), pq.Array(types),
		pq.Array(nonNil(r.Sources)), r.TargetMinutes, r.Clock, r.Starts, r.Rank, softDelete.By(by)}
	q := `UPDATE sla_rules SET name = $3, priorities = $4, labels = $5, type_ids = $6::uuid[], sources = $7,
		target_minutes = $8, clock = $9, starts = $10, rank = $11, updated_by = $12, updated_at = NOW()
		WHERE id = $1 AND project_uuid = $2 AND ` + softDelete.Live("")
	if made {
		q = `INSERT INTO sla_rules (id, project_uuid, name, priorities, labels, type_ids, sources, target_minutes, clock, starts, rank, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6::uuid[], $7, $8, $9, $10, $11, $12, $12)`
	}
	n, err := pgexec.Exec(ctx, q, args...)
	return n > 0, err
}

// AddRules writes new rules, in one statement.
func AddRules(ctx context.Context, rules []Rule, by uuid.UUID) error {
	if len(rules) == 0 {
		return nil
	}
	var rows []string
	var args []any
	for _, r := range rules {
		types := make([]string, 0, len(r.TypeIDs))
		for _, id := range r.TypeIDs {
			types = append(types, id.String())
		}
		n := len(args)
		rows = append(rows, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d::uuid[], $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10, n+11, n+12, n+12))
		args = append(args, r.ID, r.ProjectID, r.Name, pq.Array(nonNil(r.Priorities)), pq.Array(nonNil(r.Labels)), pq.Array(types),
			pq.Array(nonNil(r.Sources)), r.TargetMinutes, r.Clock, r.Starts, r.Rank, softDelete.By(by))
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO sla_rules (id, project_uuid, name, priorities, labels, type_ids, sources, target_minutes, clock, starts, rank, created_by, updated_by)
		VALUES `+strings.Join(rows, ", "), args...)
	return err
}

// RulesLockKey is the transaction lock a project's rule changes hold: its adds,
// its restores and the first change, which writes its defaults as rows.
func RulesLockKey(project uuid.UUID) string { return "sla_rules:" + project.String() }

// LockRules holds the project's rules until the transaction ends (in InTx):
// two changes at once never both write its defaults, nor both take its last
// place.
func LockRules(ctx context.Context, project uuid.UUID) error {
	return pgexec.LockXact(ctx, RulesLockKey(project))
}

// Bound is a project's most live rules, for a removed rule's restore: counted
// under the lock its adds take; past most it answers full, as an add does.
func Bound(project uuid.UUID, most int, full error) softDelete.Bound {
	return softDelete.Bound{Lock: RulesLockKey(project), Most: most, Full: full, Count: func(ctx context.Context) (int, error) {
		return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM sla_rules WHERE project_uuid = $1 AND `+softDelete.Live(""), project)
	}}
}

// RemoveRule marks the project's live rule removed, by whom (the shared soft
// delete, kind sla_rule); found is false for none. It times nothing new.
func RemoveRule(ctx context.Context, project, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.SLARule, id, by, "project_uuid = $3", project)
}

// RestoreRule brings the project's removed rule back while the project holds
// at most most live rules (else full), and answers whether one was removed.
func RestoreRule(ctx context.Context, project, id uuid.UUID, most int, full error) (bool, error) {
	return softDelete.Restore(ctx, softDelete.SLARule, id, Bound(project, most, full))
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Week is a project's work week as kept: hours (business/WeekHours JSON,
// nil for none) and zone ("" for none).
type Week struct {
	Hours []byte
	Zone  string
}

// WeekOf is the project's work week; none set is the zero Week.
func WeekOf(ctx context.Context, project uuid.UUID) (Week, error) {
	w, _, err := pgexec.QueryOne(ctx, `SELECT COALESCE(work_hours::text, ''), COALESCE(work_zone, '') FROM project_settings WHERE project_uuid = $1`,
		func(r pgexec.Row) (Week, error) {
			var w Week
			var hours string
			err := r.Scan(&hours, &w.Zone)
			if hours != "" {
				w.Hours = []byte(hours)
			}
			return w, err
		}, project)
	return w, err
}

// SetWeek keeps the project's work week: hours (nil for none) in zone.
func SetWeek(ctx context.Context, project uuid.UUID, hours []byte, zone string, by uuid.UUID) error {
	var h any
	if hours != nil {
		h = string(hours)
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO project_settings (project_uuid, work_hours, work_zone, set_by, updated_at)
		VALUES ($1, $2::jsonb, NULLIF($3, ''), $4, NOW())
		ON CONFLICT (project_uuid) DO UPDATE SET work_hours = EXCLUDED.work_hours, work_zone = EXCLUDED.work_zone,
			set_by = EXCLUDED.set_by, updated_at = NOW()`, project, h, zone, softDelete.By(by))
	return err
}

// KeepZone sets the project's zone when it has none yet; one it has stands.
func KeepZone(ctx context.Context, project uuid.UUID, zone string, by uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO project_settings (project_uuid, work_zone, set_by, updated_at)
		VALUES ($1, NULLIF($2, ''), $3, NOW())
		ON CONFLICT (project_uuid) DO UPDATE SET work_zone = EXCLUDED.work_zone, updated_at = NOW()
		WHERE project_settings.work_zone IS NULL`, project, zone, softDelete.By(by))
	return err
}

const timerColumns = `task_uuid, project_uuid, rule_id, source, started_at, due_at, risk_at, state, met_at, warned_at, breach_told_at, ended_at, COALESCE(ended_reason, '')`

func scanTimer(r pgexec.Row) (Timer, error) {
	var t Timer
	var rule uuid.NullUUID
	err := r.Scan(&t.TaskID, &t.ProjectID, &rule, &t.Source, &t.StartedAt, &t.DueAt, &t.RiskAt, &t.State, &t.MetAt,
		&t.WarnedAt, &t.BreachToldAt, &t.EndedAt, &t.EndedReason)
	if rule.Valid {
		t.RuleID = &rule.UUID
	}
	return t, err
}

// TimerOf is the task's timer, found false for none.
func TimerOf(ctx context.Context, task uuid.UUID) (Timer, bool, error) {
	return pgexec.QueryOne(ctx, `SELECT `+timerColumns+` FROM task_slas WHERE task_uuid = $1`, scanTimer, task)
}

// TimersRead is the tasks' timers, by task, in one statement, as a read a
// caller pipelines with others (pgexec.Batch); timers is what it read, once
// the batch has run.
func TimersRead(tasks []uuid.UUID) (read *pgexec.Read, timers func() (map[uuid.UUID]Timer, error)) {
	var rows []Timer
	read = pgexec.Rows(&rows, scanTimer, `SELECT `+timerColumns+` FROM task_slas WHERE task_uuid = ANY($1::uuid[])`, pq.Array(tasks))
	return read, func() (map[uuid.UUID]Timer, error) {
		out := make(map[uuid.UUID]Timer, len(rows))
		for _, t := range rows {
			out[t.TaskID] = t
		}
		return out, read.Err
	}
}

// TimersIn is the project's timers, in one read.
func TimersIn(ctx context.Context, project uuid.UUID) ([]Timer, error) {
	return pgexec.Query(ctx, `SELECT `+timerColumns+` FROM task_slas WHERE project_uuid = $1`, scanTimer, project)
}

// PutTimer keeps t as the task's timer. A due moved from the one told about
// is told again (warned_at and breach_told_at clear), so an alert names the
// due it was for.
func PutTimer(ctx context.Context, t Timer) error {
	var rule any
	if t.RuleID != nil {
		rule = *t.RuleID
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO task_slas (task_uuid, project_uuid, rule_id, source, started_at, due_at, risk_at, state, met_at, ended_at, ended_reason, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), NOW())
		ON CONFLICT (task_uuid) DO UPDATE SET project_uuid = EXCLUDED.project_uuid, rule_id = EXCLUDED.rule_id,
			started_at = EXCLUDED.started_at, due_at = EXCLUDED.due_at, risk_at = EXCLUDED.risk_at, state = EXCLUDED.state,
			met_at = EXCLUDED.met_at, ended_at = EXCLUDED.ended_at, ended_reason = EXCLUDED.ended_reason,
			warned_at = CASE WHEN task_slas.due_at IS NOT DISTINCT FROM EXCLUDED.due_at THEN task_slas.warned_at END,
			breach_told_at = CASE WHEN task_slas.due_at IS NOT DISTINCT FROM EXCLUDED.due_at THEN task_slas.breach_told_at END,
			updated_at = NOW()`,
		t.TaskID, t.ProjectID, rule, t.Source, t.StartedAt, t.DueAt, t.RiskAt, t.State, t.MetAt, t.EndedAt, t.EndedReason)
	return err
}

// Alert is a timer whose alert is due: at risk, or past due (Breach).
type Alert struct {
	TaskID    uuid.UUID
	ProjectID uuid.UUID
	DueAt     time.Time
	Breach    bool
}

// alertColumns are an alert's columns as of $1, the time a sweep tells at.
const alertColumns = `task_uuid, project_uuid, due_at, due_at <= $1`

func scanAlert(r pgexec.Row) (Alert, error) {
	var a Alert
	err := r.Scan(&a.TaskID, &a.ProjectID, &a.DueAt, &a.Breach)
	return a, err
}

// dueNow is a running timer with an alert untold at $1: past due and its
// breach untold, or at risk, not yet due, and its warning untold.
const dueNow = `state = 'running' AND ended_at IS NULL AND due_at IS NOT NULL
	AND ((due_at <= $1 AND breach_told_at IS NULL) OR (risk_at <= $1 AND due_at > $1 AND warned_at IS NULL))`

// Due is at most most timers whose alert is due at now and untold, soonest
// due first: one read.
func Due(ctx context.Context, now time.Time, most int) ([]Alert, error) {
	return pgexec.Query(ctx, `SELECT `+alertColumns+` FROM task_slas WHERE `+dueNow+` ORDER BY due_at LIMIT $2`, scanAlert, now, most)
}

// Claim takes the tasks' alerts still due and untold at now for one sweep,
// and answers those it took: only one UPDATE finds each untold (as the
// check-ins' Claim), so two sweeps, or two servers, never tell one twice. A
// breach marks the warning told too: past due is one alert.
func Claim(ctx context.Context, tasks []uuid.UUID, now time.Time) ([]Alert, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `UPDATE task_slas SET breach_told_at = CASE WHEN due_at <= $1 THEN $1 ELSE breach_told_at END,
		warned_at = COALESCE(warned_at, $1)
		WHERE task_uuid = ANY($2::uuid[]) AND `+dueNow+`
		RETURNING `+alertColumns, scanAlert, now, pq.Array(tasks))
}
