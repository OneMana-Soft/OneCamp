package business

// Doc AI's record (docs programme B9, ai-search-io P1-2): every accepted AI edit
// to a doc, every agent edit and every refusal is written to the hash-chained
// record (business/AdminAudit), where the person's own AI activity and "Prove
// it" read it, and where "AI activity on this doc" finds it by the doc.
//
// The words are "recorded" and "you can check it", never more than that.
//
// WHAT A ROW HOLDS. Never the doc's text: the action, the model and whether it
// ran on the workspace's own network, how much changed, and the hashes of the
// text before and after, which show what changed without keeping it.
//
// THE ACTIONS, all under "ai.doc." so one read finds a doc's whole AI history:
//   ai.doc.edit.accepted        a person accepted an AI edit
//   ai.doc.read.refused         doc AI was asked about a doc the person can't read
//   ai.doc.tasks.created        tasks were made from a doc's action items
//   ai.doc.tasks.refused        ... in a project the person can't add tasks to
//   ai.doc.agent_edit.allowed   an agent wrote into a doc (docEdit.go)
//   ai.doc.agent_edit.refused   an agent was stopped from writing into one
//   ai.doc.agent_edit.reverted  someone reverted an agent's edit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The record's actions for docs.
const (
	ActionDocAIEdit           = "ai.doc.edit.accepted"
	ActionDocAIReadRefused    = "ai.doc.read.refused"
	ActionDocAITasksCreated   = "ai.doc.tasks.created"
	ActionDocAITasksRefused   = "ai.doc.tasks.refused"
	ActionDocAgentEditAllowed = "ai.doc.agent_edit.allowed"
	ActionDocAgentEditRefused = "ai.doc.agent_edit.refused"
	ActionDocAgentEditRevert  = "ai.doc.agent_edit.reverted"
)

// docAIActionPrefix is what every doc row's action starts with.
const docAIActionPrefix = "ai.doc."

var (
	// ErrDocAINotFound is a doc that doesn't exist, is in the trash, or that
	// the person can't open: one answer for all three, so private docs can't
	// be found by asking.
	ErrDocAINotFound = errors.New("there's no such doc, or you can't open it")
	// ErrDocAICantEdit is a doc the person can read but not change (a viewer,
	// a commenter, or a doc locked by its owner).
	ErrDocAICantEdit = errors.New("you can view this doc but not edit it")
)

// recordEntry writes one row: a seam, so the rules are tested without the
// store. Synchronous, so a caller can refuse what it couldn't record.
var recordEntry = func(ctx context.Context, actorID *uuid.UUID, actorEmail, actorKind, action, summary string, meta map[string]interface{}) error {
	return auditBusiness.RecordForPrincipal(ctx, actorID, actorEmail, actorKind, action, auditBusiness.CategoryAgent, summary, meta)
}

// resolveDoc is the person's decision on a doc: a seam over the Access Resolver.
var resolveDoc = func(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
	return accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
}

// docTitle is a doc's title, as the person may read it: a seam.
var docTitle = func(ctx context.Context, docUUID, userUID string) string {
	d, err := docBusiness.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil || d == nil {
		return ""
	}
	return strings.TrimSpace(d.Title)
}

// DocTitleFor is a doc's title, for a person who may read it.
func DocTitleFor(ctx context.Context, docUUID, userUID string) string {
	return docTitle(ctx, docUUID, userUID)
}

func actorOf(u *userModels.UserInfo) (*uuid.UUID, string) {
	id := u.UserPostgresInfo.Id
	if id == uuid.Nil {
		if parsed, err := uuid.Parse(u.UserDgraphInfo.Uuid); err == nil {
			id = parsed
		}
	}
	if id == uuid.Nil {
		return nil, u.UserPostgresInfo.EmailID
	}
	return &id, u.UserPostgresInfo.EmailID
}

// canEditDecision: editing needs edit, and a hard lock (D-12) leaves only full
// access able to change the doc.
func canEditDecision(d accessBusiness.Decision) bool {
	return d.Level >= accessBusiness.LevelEdit && (!d.Locked || d.Level >= accessBusiness.LevelFull)
}

// decideDoc is someone's decision on a doc against what they need: nil, or
// ErrDocAINotFound (missing, in the trash, or not theirs), or ErrDocAICantEdit.
func decideDoc(ctx context.Context, who accessBusiness.Subject, docUUID string, need accessBusiness.Level) (accessBusiness.Decision, error) {
	if _, err := uuid.Parse(strings.TrimSpace(docUUID)); err != nil {
		return accessBusiness.Decision{}, ErrDocAINotFound
	}
	d, err := resolveDoc(ctx, who, strings.TrimSpace(docUUID))
	switch {
	case errors.Is(err, docBusiness.ErrNotFound):
		return d, ErrDocAINotFound
	case err != nil:
		return d, err
	case d.Deleted || d.Level < accessBusiness.LevelView:
		return d, ErrDocAINotFound
	case need >= accessBusiness.LevelEdit && !canEditDecision(d):
		return d, ErrDocAICantEdit
	case d.Level < need:
		return d, ErrDocAICantEdit
	}
	return d, nil
}

// CheckDocForAI is the person's level on a doc, refused with ErrDocAINotFound
// when they can't read it (or it is gone) and with ErrDocAICantEdit when need
// is edit and they may not change it. A refusal is recorded.
func CheckDocForAI(ctx context.Context, u *userModels.UserInfo, docUUID string, need accessBusiness.Level) (accessBusiness.Decision, error) {
	d, err := decideDoc(ctx, accessBusiness.SubjectFor(ctx, u), docUUID, need)
	if errors.Is(err, ErrDocAINotFound) || errors.Is(err, ErrDocAICantEdit) {
		RecordDocAIRefusal(ctx, u, docUUID, ActionDocAIReadRefused, err.Error())
	}
	return d, err
}

// checkDocQuietly is CheckDocForAI for a mere look (a listing, the starters):
// refused alike, but recorded nowhere, so polling a doc's id adds nothing to
// the record.
func checkDocQuietly(ctx context.Context, u *userModels.UserInfo, docUUID string, need accessBusiness.Level) (accessBusiness.Decision, error) {
	return decideDoc(ctx, accessBusiness.SubjectFor(ctx, u), docUUID, need)
}

// RecordDocAIRefusal records that doc AI was refused for u on a doc. Best
// effort: the refusal stands whether or not the row is written.
func RecordDocAIRefusal(ctx context.Context, u *userModels.UserInfo, docUUID, action, reason string) {
	id, email := actorOf(u)
	meta := map[string]interface{}{"doc_uuid": strings.TrimSpace(docUUID), "reason": reason}
	if err := recordEntry(context.WithoutCancel(ctx), id, email, auditBusiness.ActorHuman, action, "Doc AI refused: "+reason, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI doc AI refusal on %s could not be recorded: %v", docUUID, err)
	}
}

// DocAIInvalidError is a record request that can't be taken as sent; its
// message is said to the person.
type DocAIInvalidError struct{ Msg string }

func (e *DocAIInvalidError) Error() string { return e.Msg }

// DocAIEdit is an AI edit someone accepted into a doc.
type DocAIEdit struct {
	DocUUID string `json:"doc_uuid"`
	// Action is what was run ("improve", "continue", "summarize"…).
	Action string `json:"action"`
	// Model and Local say which model wrote it and whether it ran on the
	// workspace's own network. The server sets them from its own resolution of
	// the person's model (controllers/AI aiDocApplied.go); what a client sends
	// here is not taken.
	Model string `json:"model"`
	Local bool   `json:"local"`
	// Changes is how many changes were accepted (all, or some of them).
	Changes int `json:"changes"`
	// Of is how many the proposal had.
	Of int `json:"of"`
	// Inserted: the answer went in below instead of replacing the text.
	Inserted bool `json:"inserted"`
	// BeforeHash and AfterHash are SHA-256 hashes of the text before and
	// after, from the editor: what changed, without the text.
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
	// Chars is how many characters the accepted text holds.
	Chars int `json:"chars"`
}

func hexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// RecordDocAIEdit records an accepted AI edit, after checking that the person
// may edit the doc. Unlike a refusal, a failure to record is returned: the
// editor says the edit went unrecorded rather than claiming it was recorded.
func RecordDocAIEdit(ctx context.Context, u *userModels.UserInfo, e DocAIEdit) error {
	a, ok := LookupDocAction(e.Action)
	if !ok {
		return &DocAIInvalidError{"That isn't an action the AI bar knows."}
	}
	if (e.BeforeHash != "" && !hexHash(e.BeforeHash)) || (e.AfterHash != "" && !hexHash(e.AfterHash)) {
		return &DocAIInvalidError{"The text's fingerprints must be SHA-256, written in hex."}
	}
	if e.Changes < 0 || e.Of < 0 || e.Chars < 0 || (e.Of > 0 && e.Changes > e.Of) {
		return &DocAIInvalidError{"The numbers of changes don't add up."}
	}
	if _, err := CheckDocForAI(ctx, u, e.DocUUID, accessBusiness.LevelEdit); err != nil {
		return err
	}
	model := helpers.TruncateRunes(strings.TrimSpace(e.Model), 80)
	meta := map[string]interface{}{
		"doc_uuid": strings.TrimSpace(e.DocUUID),
		"action":   a.Name,
		"model":    model,
		"local":    e.Local,
		"changes":  e.Changes,
		"of":       e.Of,
		"inserted": e.Inserted,
		"chars":    e.Chars,
	}
	if e.BeforeHash != "" {
		meta["before_hash"] = e.BeforeHash
	}
	if e.AfterHash != "" {
		meta["after_hash"] = e.AfterHash
	}
	id, email := actorOf(u)
	return recordEntry(ctx, id, email, auditBusiness.ActorHuman, ActionDocAIEdit, docAIEditSummary(a.Name, model, e), meta)
}

// docAIEditSummary is the row's line: "Accepted an AI rewrite (2 of 3
// changes), written by qwen3 4B on your server". Where it was written is said
// only with the model that wrote it: an editor that didn't know the model
// doesn't know that either. Pure.
func docAIEditSummary(action, model string, e DocAIEdit) string {
	verb := "Accepted"
	if e.Inserted {
		verb = "Inserted"
	}
	s := verb + " " + docActionWord(action)
	if !e.Inserted && e.Of > 1 && e.Changes < e.Of {
		s += fmt.Sprintf(" (%d of %d changes)", e.Changes, e.Of)
	}
	if model == "" {
		return s
	}
	where := "on your server"
	if !e.Local {
		where = "outside your server"
	}
	return s + ", written by " + model + " " + where
}

// docActionWord names an action in a sentence.
func docActionWord(action string) string {
	switch action {
	case "edit":
		return "an AI edit"
	case "improve":
		return "an AI rewrite"
	case "fix":
		return "an AI spelling and grammar fix"
	case "shorter":
		return "an AI shortening"
	case "longer":
		return "an AI lengthening"
	case "tone":
		return "an AI change of tone"
	case "translate":
		return "an AI translation"
	case "simplify":
		return "an AI simplification"
	case "summarize":
		return "an AI summary"
	case "continue":
		return "an AI continuation"
	case "write", "draft":
		return "an AI draft"
	case "draft_from_tasks":
		return "an AI draft from open tasks"
	case "explain":
		return "an AI explanation"
	case "ask":
		return "an AI answer"
	}
	return "an AI edit"
}

// DocAIActivityItem is one row of "AI activity on this doc".
type DocAIActivityItem struct {
	At        time.Time `json:"at"`
	Action    string    `json:"action"`
	Status    string    `json:"status,omitempty"`
	Summary   string    `json:"summary"`
	Actor     string    `json:"actor,omitempty"`
	ActorKind string    `json:"actor_kind,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	AgentName string    `json:"agent_name,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	Local     *bool     `json:"local,omitempty"`
	// The row's place in the chain, so it can be checked.
	Seq       int64  `json:"seq"`
	PrevHash  string `json:"prev_hash,omitempty"`
	EntryHash string `json:"entry_hash,omitempty"`
}

// listDocAIEntries reads a doc's rows: a seam over the store.
var listDocAIEntries = auditModel.ListDocAIEntries

// DocAIActivity is the AI activity on a doc, newest first, for someone who can
// read the doc. A reader who can't open the doc sees nothing (ErrDocAINotFound).
func DocAIActivity(ctx context.Context, u *userModels.UserInfo, docUUID string, limit int) ([]DocAIActivityItem, error) {
	if _, err := checkDocQuietly(ctx, u, docUUID, accessBusiness.LevelView); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := listDocAIEntries(ctx, strings.TrimSpace(docUUID), docAIActionPrefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DocAIActivityItem, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		item := DocAIActivityItem{
			At: r.CreatedAt, Action: r.Action, Status: activityStatus(r.Action),
			Summary: r.Summary, Actor: r.ActorEmail, ActorKind: r.ActorKind,
			AgentID: metadataString(r.Metadata, metaAgentID), AgentName: metadataString(r.Metadata, metaAgentName),
			RunID: metadataString(r.Metadata, metaRunID), Model: metadataString(r.Metadata, "model"),
			Seq: r.Seq, PrevHash: r.PrevHash, EntryHash: r.EntryHash,
		}
		if l, ok := metadataBool(r.Metadata, "local"); ok {
			item.Local = &l
		}
		out = append(out, item)
	}
	return out, nil
}

// metadataBool reads one boolean field out of a row's metadata JSON.
func metadataBool(raw *string, key string) (bool, bool) {
	if raw == nil || *raw == "" {
		return false, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return false, false
	}
	v, ok := m[key].(bool)
	return v, ok
}
