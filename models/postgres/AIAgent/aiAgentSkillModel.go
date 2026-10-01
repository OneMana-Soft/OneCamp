package models

// Data-access layer for reusable agent skills (migration 109): named, shareable
// instruction modules an agent composes into its system prompt at run time.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AgentSkill is a reusable instruction module.
type AgentSkill struct {
	Id           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Instructions string    `json:"instructions"`
	CreatedBy    uuid.UUID `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// AgentCount is how many agents this skill is attached to: the blast radius
	// of editing it. Computed on the list read rather than stored, because the
	// answer changes whenever any agent is edited and a cached copy would be
	// wrong exactly when it matters.
	AgentCount int `json:"agent_count"`
}

func scanSkill(s scanner) (*AgentSkill, error) {
	var sk AgentSkill
	if err := s.Scan(&sk.Id, &sk.Name, &sk.Instructions, &sk.CreatedBy, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
		return nil, err
	}
	return &sk, nil
}

// Qualified with the `s` alias because the list query joins against
// ai_agents to count usage. Every query using these columns must alias
// ai_agent_skills as s, or Postgres rejects it at runtime with a missing
// FROM-clause entry, which no compiler or vet pass will catch.
const skillCols = `s.id, s.name, s.instructions, s.created_by, s.created_at, s.updated_at`

// CreateSkill inserts a skill and returns its id.
func CreateSkill(ctx context.Context, s *AgentSkill) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO ai_agent_skills (id, name, instructions, created_by) VALUES ($1,$2,$3,$4)`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, s.Name, s.Instructions, s.CreatedBy); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateSkill err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateSkill updates a skill's name + instructions.
func UpdateSkill(ctx context.Context, s *AgentSkill) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_agent_skills SET name=$2, instructions=$3, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, s.Id, s.Name, s.Instructions)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateSkill err: %+v", err)
	}
	return err
}

// GetSkillByID returns a single non-deleted skill, or (nil, nil).
func GetSkillByID(ctx context.Context, id uuid.UUID) (*AgentSkill, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT `+skillCols+` FROM ai_agent_skills s WHERE s.id=$1 AND s.deleted_at IS NULL`, id)
	sk, err := scanSkill(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sk, nil
}

// ListSkills returns all non-deleted skills (workspace library), newest first.
func ListSkills(ctx context.Context) ([]*AgentSkill, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	// AgentCount comes back with the list rather than from a request per skill.
	// A library of thirty skills would otherwise be thirty round trips to answer
	// a question the list is already being drawn for.
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT `+skillCols+`,
		        (SELECT COUNT(*) FROM ai_agents a
		          WHERE a.deleted_at IS NULL AND a.skill_ids ? s.id::text) AS agent_count
		   FROM ai_agent_skills s
		  WHERE s.deleted_at IS NULL
		  ORDER BY s.created_at DESC
		  LIMIT 500`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListSkills err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AgentSkill
	for rows.Next() {
		var sk AgentSkill
		if serr := rows.Scan(&sk.Id, &sk.Name, &sk.Instructions, &sk.CreatedBy,
			&sk.CreatedAt, &sk.UpdatedAt, &sk.AgentCount); serr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListSkills scan err: %+v", serr)
			return nil, serr
		}
		out = append(out, &sk)
	}
	return out, rows.Err()
}

// GetSkillsByIDs returns the named skills (non-deleted) in arbitrary order.
// Used by the runner to compose an agent's referenced skills.
func GetSkillsByIDs(ctx context.Context, ids []uuid.UUID) ([]*AgentSkill, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT `+skillCols+` FROM ai_agent_skills s WHERE s.id = ANY($1) AND s.deleted_at IS NULL`, pqUUIDArray(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetSkillsByIDs err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AgentSkill
	for rows.Next() {
		sk, serr := scanSkill(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// SoftDeleteSkill marks a skill deleted. Agents referencing it simply omit it.
func SoftDeleteSkill(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE ai_agent_skills SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteSkill err: %+v", err)
	}
	return err
}

// pqUUIDArray renders a uuid slice as a Postgres uuid[] literal for ANY($1).
func pqUUIDArray(ids []uuid.UUID) string {
	var b []byte
	b = append(b, '{')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, id.String()...)
	}
	b = append(b, '}')
	return string(b)
}

// SkillRevision is one version of a skill's text, with who wrote it and why.
type SkillRevision struct {
	Id           uuid.UUID  `json:"id"`
	SkillId      uuid.UUID  `json:"skill_id"`
	Name         string     `json:"name"`
	Instructions string     `json:"instructions"`
	Note         string     `json:"note"`
	EditedBy     *uuid.UUID `json:"edited_by,omitempty"`
	EditedByName string     `json:"edited_by_name,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// CreateSkillRevision records a version of a skill.
//
// Called for every version including the first, so the history is a list of
// what the skill has said rather than a list of diffs to reconstruct.
func CreateSkillRevision(ctx context.Context, r *SkillRevision) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `INSERT INTO ai_agent_skill_revisions (skill_id, name, instructions, note, edited_by)
	           VALUES ($1,$2,$3,$4,$5)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, r.SkillId, r.Name, r.Instructions, r.Note, r.EditedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateSkillRevision err: %+v", err)
	}
	return err
}

// ListSkillRevisions returns a skill's versions, newest first.
//
// Ordered by created_at, which is a total order here because each revision is
// written by its own request in its own transaction. Two revisions written
// inside ONE transaction would share a timestamp, since Postgres NOW() is
// transaction time, and would sort arbitrarily. No code path does that, and if
// one ever does it needs a tiebreaker rather than a hope.
//
// Joined to users for the editor's name, left so a revision written by someone
// who has since left the workspace still appears with its text and date.
func ListSkillRevisions(ctx context.Context, skillID uuid.UUID, limit int) ([]*SkillRevision, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT r.id, r.skill_id, r.name, r.instructions, r.note, r.edited_by,
	                  COALESCE(u.display_name, ''), r.created_at
	           FROM ai_agent_skill_revisions r
	           LEFT JOIN users u ON u.id = r.edited_by
	           WHERE r.skill_id = $1
	           ORDER BY r.created_at DESC
	           LIMIT $2`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, skillID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListSkillRevisions err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := []*SkillRevision{}
	for rows.Next() {
		var r SkillRevision
		var editedBy uuid.NullUUID
		if err := rows.Scan(&r.Id, &r.SkillId, &r.Name, &r.Instructions, &r.Note, &editedBy, &r.EditedByName, &r.CreatedAt); err != nil {
			helpers.LogErrorWithContext(ctx, "models/ListSkillRevisions scan err: %+v", err)
			return nil, err
		}
		if editedBy.Valid {
			id := editedBy.UUID
			r.EditedBy = &id
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetSkillRevision loads one revision, or (nil, nil) when it is not this
// skill's. Scoped by skill id so a revision id from another skill cannot be
// used to write text across the library.
func GetSkillRevision(ctx context.Context, skillID, revisionID uuid.UUID) (*SkillRevision, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT id, skill_id, name, instructions, note, edited_by, '', created_at
	           FROM ai_agent_skill_revisions WHERE id = $1 AND skill_id = $2`
	var r SkillRevision
	var editedBy uuid.NullUUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, revisionID, skillID).
		Scan(&r.Id, &r.SkillId, &r.Name, &r.Instructions, &r.Note, &editedBy, &r.EditedByName, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetSkillRevision err: %+v", err)
		return nil, err
	}
	if editedBy.Valid {
		id := editedBy.UUID
		r.EditedBy = &id
	}
	return &r, nil
}

// SkillUsage names the agents that reference a skill.
//
// The blast radius of an edit. A skill is one text attached to many agents, so
// changing it changes all of them on their next run, and the person editing it
// currently cannot see how many that is.
// MarkAgentsUsingSkillChanged makes every agent that composes a skill due for
// re-evaluation, and returns how many were marked.
//
// The eval watch reruns an agent's suite when the AGENT's updated_at moves past
// its last measurement. Editing a shared skill changes what an agent is told
// just as much as editing its own instructions, and moved nothing: the skill
// lives in another table. So the one edit that reaches many agents at once was
// the single edit that never triggered a re-measurement, and the badge went on
// reporting a pass rate for a prompt that no longer existed.
//
// Bumping updated_at rather than adding a parallel "due" flag is deliberate. It
// reuses the whole existing mechanism, including the staleness marker the badge
// already derives from the same comparison, so one write fixes the rerun AND
// stops the badge claiming to describe the current agent.
//
// Uses the same containment test as SkillUsage, so the set of agents marked is
// exactly the set the blast-radius warning showed the person before they saved.
func MarkAgentsUsingSkillChanged(ctx context.Context, skillID uuid.UUID) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_agents SET updated_at = NOW()
	           WHERE deleted_at IS NULL AND skill_ids ? $1`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, skillID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkAgentsUsingSkillChanged err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func SkillUsage(ctx context.Context, skillID uuid.UUID) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	// skill_ids is a jsonb array of id strings; `?` asks whether it contains
	// one, which is an index-friendly containment test rather than a scan of
	// every agent's array in Go.
	const q = `SELECT name FROM ai_agents
	           WHERE deleted_at IS NULL AND skill_ids ? $1
	           ORDER BY name`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, skillID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SkillUsage err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
