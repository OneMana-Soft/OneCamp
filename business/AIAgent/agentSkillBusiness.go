package business

// Reusable agent skills: the workspace-shared library of named instruction
// modules an agent composes into its prompt. Managed under the same agent.manage
// capability as the rest of the builder; edit/delete restricted to the creator
// or an admin (consistent with agent ownership), while any capability holder may
// create and list (a shared library).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// SkillInput is the create/update payload for a skill.
type SkillInput struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	// Note is why this edit was made, stored with the revision. Optional,
	// because requiring it would train people to type "update", and an audit
	// trail full of "update" is worse than one with gaps: it looks answered.
	Note string `json:"note,omitempty"`
}

func validateSkill(in *SkillInput) (string, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if len(name) > maxSkillNameLen {
		return "", "", fmt.Errorf("name is too long")
	}
	body := strings.TrimSpace(in.Instructions)
	if body == "" {
		return "", "", fmt.Errorf("instructions are required")
	}
	if len(body) > maxSkillBodyLen {
		return "", "", fmt.Errorf("instructions are too long")
	}
	return name, body, nil
}

// CreateSkill adds a skill to the workspace library.
func CreateSkill(ctx context.Context, in SkillInput, actor Actor) (*model.AgentSkill, error) {
	name, body, err := validateSkill(&in)
	if err != nil {
		return nil, err
	}
	id, err := model.CreateSkill(ctx, &model.AgentSkill{Name: name, Instructions: body, CreatedBy: actor.UserID})
	if err != nil {
		return nil, fmt.Errorf("failed to create skill")
	}
	// The version it was created with, so the history is a complete list of what
	// this skill has said rather than everything after the first edit.
	recordSkillRevision(ctx, id, name, body, "Created", actor.UserID)
	return model.GetSkillByID(ctx, id)
}

// UpdateSkill edits a skill (creator or admin only).
func UpdateSkill(ctx context.Context, id uuid.UUID, in SkillInput, actor Actor) (*model.AgentSkill, error) {
	existing, err := model.GetSkillByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load skill")
	}
	if existing == nil {
		return nil, errNotFound
	}
	if !actor.IsAdmin && existing.CreatedBy != actor.UserID {
		return nil, errForbidden
	}
	name, body, verr := validateSkill(&in)
	if verr != nil {
		return nil, verr
	}
	unchanged := existing.Name == name && existing.Instructions == body
	existing.Name = name
	existing.Instructions = body
	if uerr := model.UpdateSkill(ctx, existing); uerr != nil {
		return nil, fmt.Errorf("failed to update skill")
	}
	// No revision for a save that changed nothing: a history padded with
	// identical entries is one nobody reads.
	if !unchanged {
		recordSkillRevision(ctx, id, name, body, in.Note, actor.UserID)
	}
	return model.GetSkillByID(ctx, id)
}

// maxSkillNoteLen bounds the reason stored with a revision. Long enough for a
// sentence explaining a change, short enough that the history stays scannable.
const maxSkillNoteLen = 500

// recordSkillRevision stores a version. Best-effort by contract: losing the
// history entry is bad, and refusing the edit the user asked for because the
// history could not be written is worse.
func recordSkillRevision(ctx context.Context, skillID uuid.UUID, name, instructions, note string, editedBy uuid.UUID) {
	note = strings.TrimSpace(note)
	if len(note) > maxSkillNoteLen {
		note = note[:maxSkillNoteLen]
	}
	by := editedBy
	if err := model.CreateSkillRevision(ctx, &model.SkillRevision{
		SkillId:      skillID,
		Name:         name,
		Instructions: instructions,
		Note:         note,
		EditedBy:     &by,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/recordSkillRevision failed for %s: %+v", skillID, err)
	}

	// Every agent composing this skill is now running a prompt nobody has
	// measured. This is what connects the skill library to the eval harness.
	//
	// The watch reruns an agent's suite when the AGENT's updated_at moves past
	// its last measurement, and a skill lives in another table, so the one edit
	// that reaches many agents at once was the single edit that never triggered a
	// re-measurement. The badge went on reporting a pass rate for a prompt that
	// no longer existed.
	//
	// Best-effort for the same reason the revision above is: losing the signal is
	// bad, and refusing an edit somebody asked for because a bookkeeping write
	// failed is worse.
	if n, merr := model.MarkAgentsUsingSkillChanged(ctx, skillID); merr != nil {
		helpers.LogErrorWithContext(ctx, "business/recordSkillRevision could not mark agents for re-evaluation: %+v", merr)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx, "skill %s changed: %d agent(s) marked for re-evaluation", skillID, n)
	}
}

// SkillUsage lists the agents a skill is attached to: the blast radius of
// editing it. Readable by anyone who can see the library, because the point is
// to be seen BEFORE an edit, not to be discovered after one.
func SkillUsage(ctx context.Context, id uuid.UUID) ([]string, error) {
	return model.SkillUsage(ctx, id)
}

// SkillRevisions returns a skill's version history, newest first.
func SkillRevisions(ctx context.Context, id uuid.UUID, limit int) ([]*model.SkillRevision, error) {
	return model.ListSkillRevisions(ctx, id, limit)
}

// RevertSkill restores a previous version.
//
// Writes a NEW revision rather than deleting the ones after it: a rollback is
// something that happened, and erasing what it undid would leave a history that
// cannot explain itself. Same permission as editing, because that is what it
// is.
func RevertSkill(ctx context.Context, id, revisionID uuid.UUID, actor Actor) (*model.AgentSkill, error) {
	existing, err := model.GetSkillByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load skill")
	}
	if existing == nil {
		return nil, errNotFound
	}
	if !actor.IsAdmin && existing.CreatedBy != actor.UserID {
		return nil, errForbidden
	}
	// Scoped by skill id, so a revision belonging to another skill cannot be
	// used to write its text into this one.
	rev, err := model.GetSkillRevision(ctx, id, revisionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load revision")
	}
	if rev == nil {
		return nil, errNotFound
	}

	existing.Name = rev.Name
	existing.Instructions = rev.Instructions
	if uerr := model.UpdateSkill(ctx, existing); uerr != nil {
		return nil, fmt.Errorf("failed to revert skill")
	}
	recordSkillRevision(ctx, id, rev.Name, rev.Instructions,
		fmt.Sprintf("Reverted to the version from %s", rev.CreatedAt.Format("2 Jan 2006, 15:04")), actor.UserID)
	return model.GetSkillByID(ctx, id)
}

// DeleteSkill removes a skill (creator or admin only). Agents referencing it
// simply omit it on their next run.
func DeleteSkill(ctx context.Context, id uuid.UUID, actor Actor) error {
	existing, err := model.GetSkillByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to load skill")
	}
	if existing == nil {
		return errNotFound
	}
	if !actor.IsAdmin && existing.CreatedBy != actor.UserID {
		return errForbidden
	}
	return model.SoftDeleteSkill(ctx, id)
}

// ListSkills returns the workspace skill library.
func ListSkills(ctx context.Context) ([]*model.AgentSkill, error) {
	return model.ListSkills(ctx)
}

// buildSkillsPrompt composes an agent's referenced skills into a stable,
// ordered prompt block, additive to its base instructions. Edits to a skill
// take effect on the next run (no per-agent copy). Returns "" when none.
// skillIDsOf parses an agent's skill references.
//
// Extracted because two callers need it now and they must agree: the prompt
// builder, which decides what the agent is told, and the regression notice,
// which decides which skills to blame. A second copy of this parsing is a
// second answer to "which skills does this agent use".
//
// Malformed or unparseable ids are skipped rather than failing the whole list:
// one bad entry should not silently strip an agent of every skill it has.
func skillIDsOf(agent *model.AiAgent) []uuid.UUID {
	if agent == nil {
		return nil
	}
	raw := strings.TrimSpace(agent.SkillIds)
	if raw == "" || raw == "[]" {
		return nil
	}
	var idStrs []string
	if err := json.Unmarshal([]byte(raw), &idStrs); err != nil {
		return nil
	}
	out := make([]uuid.UUID, 0, len(idStrs))
	for _, s := range idStrs {
		if id, perr := uuid.Parse(strings.TrimSpace(s)); perr == nil {
			out = append(out, id)
		}
	}
	return out
}

// SkillFingerprint records that a skill was in a run's prompt, and what it said
// at the time.
//
// The fingerprint rather than the text, because a skill is shared and editable:
// storing the id alone would let an edit months later silently change what a
// past run appears to have been asked, and storing the text would copy the
// library into every run that used it. The fingerprint is small, is not content,
// and can be matched back against the revision history to name the version.
type SkillFingerprint struct {
	Id   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	SHA  string    `json:"sha256"`
}

// skillsUsedJSON renders fingerprints for storage. An empty set is "[]" rather
// than "", because the column is NOT NULL and because "this run used no skills"
// is a fact worth recording plainly.
func skillsUsedJSON(fps []SkillFingerprint) string {
	if len(fps) == 0 {
		return "[]"
	}
	b, err := json.Marshal(fps)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func buildSkillsPrompt(ctx context.Context, agent *model.AiAgent) (string, []SkillFingerprint) {
	raw := strings.TrimSpace(agent.SkillIds)
	if raw == "" || raw == "[]" {
		return "", nil
	}
	var idStrs []string
	if err := json.Unmarshal([]byte(raw), &idStrs); err != nil || len(idStrs) == 0 {
		return "", nil
	}
	ids := make([]uuid.UUID, 0, len(idStrs))
	order := make(map[uuid.UUID]int, len(idStrs))
	for i, s := range idStrs {
		if id, perr := uuid.Parse(strings.TrimSpace(s)); perr == nil {
			ids = append(ids, id)
			order[id] = i
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	skills, err := model.GetSkillsByIDs(ctx, ids)
	if err != nil || len(skills) == 0 {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/buildSkillsPrompt load err: %+v", err)
		}
		return "", nil
	}
	// Preserve the agent's configured order (GetSkillsByIDs returns arbitrary).
	sortByConfiguredOrder(skills, order)

	var b strings.Builder
	fps := make([]SkillFingerprint, 0, len(skills))
	b.WriteString("\n\nSkills (reusable instructions you should follow):\n")
	for _, sk := range skills {
		body := strings.TrimSpace(sk.Instructions)
		b.WriteString("\n### " + strings.TrimSpace(sk.Name) + "\n")
		b.WriteString(body)
		b.WriteString("\n")
		// Fingerprint the text that went INTO the prompt, not the row as loaded,
		// so the record matches what the model was actually shown.
		fps = append(fps, SkillFingerprint{Id: sk.Id, Name: sk.Name, SHA: helpers.SHA256Hex(body)})
	}
	return b.String(), fps
}

// sortByConfiguredOrder stable-orders skills by the agent's configured index.
func sortByConfiguredOrder(skills []*model.AgentSkill, order map[uuid.UUID]int) {
	for i := 1; i < len(skills); i++ {
		for j := i; j > 0 && order[skills[j].Id] < order[skills[j-1].Id]; j-- {
			skills[j], skills[j-1] = skills[j-1], skills[j]
		}
	}
}
