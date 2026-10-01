// Package business (Marketplace) is the management + install layer for
// shareable templates (agents, workflows, tables) (migration 93, simplified in
// 95).
//
// A template stores the create-input for its kind as a stable JSON payload.
// Install instantiates a fresh copy in the installer's workspace by replaying
// that payload through the SAME business Create functions the app uses, AS the
// installing user — so an install can never create something the user could
// not create by hand. For kinds that are capability-gated in the app (agents,
// workflows), install re-checks that capability.
package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	authz "github.com/akashc777/OneCamp/business/Authz"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	marketplaceDomain "github.com/akashc777/OneCamp/domain/Marketplace"
	capabilityModels "github.com/akashc777/OneCamp/models/postgres/Capability"
	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	maxNameLen        = 120
	maxDescriptionLen = 2000
	maxIconLen        = 64
	maxPayloadBytes   = 256 * 1024
)

var (
	errForbidden = fmt.Errorf("not authorized")
	errNotFound  = fmt.Errorf("template not found")
)

// IsForbidden / IsNotFound let controllers map to HTTP codes.
func IsForbidden(err error) bool { return err == errForbidden }
func IsNotFound(err error) bool  { return err == errNotFound }

// PublishInput is the payload for publishing a template.
type PublishInput struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Icon        string          `json:"icon"`
	Payload     json.RawMessage `json:"payload"`
}

// InstallResult tells the FE what was created and where to find it.
type InstallResult struct {
	Kind     string `json:"kind"`
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// Publish validates and stores a new template owned by the user.
func Publish(ctx context.Context, in PublishInput, createdBy uuid.UUID) (*model.Template, error) {
	kind := strings.TrimSpace(in.Kind)
	if !model.ValidKind(kind) {
		return nil, fmt.Errorf("invalid template kind")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("name is too long")
	}
	if len(in.Description) > maxDescriptionLen {
		return nil, fmt.Errorf("description is too long")
	}
	if len(in.Icon) > maxIconLen {
		return nil, fmt.Errorf("icon is too long")
	}
	payload := strings.TrimSpace(string(in.Payload))
	if payload == "" || payload == "null" {
		payload = "{}"
	}
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("template payload is too large")
	}
	// Validate the payload is well-formed JSON and decodes for its kind, so a
	// broken template can never be published (and thus never fail at install).
	if _, err := decodeForKind(kind, json.RawMessage(payload)); err != nil {
		return nil, fmt.Errorf("template payload is not valid for kind %s: %w", kind, err)
	}

	id, err := model.CreateTemplate(ctx, kind, name, optStr(in.Description), optStr(in.Icon), payload, createdBy)
	if err != nil {
		return nil, fmt.Errorf("failed to publish template")
	}
	return model.GetTemplate(ctx, id)
}

// List returns listed templates, optionally filtered by kind.
func List(ctx context.Context, kind string) ([]*model.Template, error) {
	if kind != "" && !model.ValidKind(kind) {
		return nil, fmt.Errorf("invalid kind")
	}
	return marketplaceDomain.ListTemplates(ctx, kind)
}

// Get returns a single listed template.
func Get(ctx context.Context, id uuid.UUID) (*model.Template, error) {
	t, err := model.GetTemplate(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load template")
	}
	if t == nil {
		return nil, errNotFound
	}
	return t, nil
}

// Unpublish unlists a template the user owns (admins may unlist any).
func Unpublish(ctx context.Context, id uuid.UUID, userInfo *userModels.UserInfo) error {
	if err := model.SoftDeleteTemplate(ctx, id, userInfo.UserPostgresInfo.Id, userInfo.UserPostgresInfo.IsAdmin); err != nil {
		return errNotFound
	}
	return nil
}

// Install instantiates a template in the installer's workspace and returns a
// reference to the new entity. Capability-gated kinds re-check the capability.
func Install(ctx context.Context, id uuid.UUID, userInfo *userModels.UserInfo) (*InstallResult, error) {
	t, err := model.GetTemplate(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load template")
	}
	if t == nil {
		return nil, errNotFound
	}

	userID := userInfo.UserPostgresInfo.Id
	var res *InstallResult

	switch t.Kind {
	case model.KindAgent:
		if !authz.Can(ctx, userInfo, capabilityModels.CapAgentManage) {
			return nil, errForbidden
		}
		res, err = installAgent(ctx, t.Payload, userID)
	case model.KindWorkflow:
		if !authz.Can(ctx, userInfo, capabilityModels.CapWorkflowManage) {
			return nil, errForbidden
		}
		res, err = installWorkflow(ctx, t.Payload, userID)
	case model.KindTable:
		res, err = installTable(ctx, t.Payload, userInfo)
	default:
		return nil, fmt.Errorf("unsupported template kind")
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// decodeForKind validates that a payload decodes into the expected shape for a
// kind (used at publish time so installs can't later fail on a bad payload).
func decodeForKind(kind string, payload json.RawMessage) (interface{}, error) {
	switch kind {
	case model.KindAgent:
		return nil, fmt.Errorf("AI agent templates are not supported on this build")
	case model.KindWorkflow:
		var in workflowPayload
		return in, json.Unmarshal(payload, &in)
	case model.KindTable:
		var in tablePayload
		return in, json.Unmarshal(payload, &in)
	default:
		return nil, fmt.Errorf("unsupported kind")
	}
}

// ── agent ──

func installAgent(ctx context.Context, payload string, userID uuid.UUID) (*InstallResult, error) {
	return nil, fmt.Errorf("AI agent templates are not supported on this build")
}

// ── workflow ──

// workflowPayload mirrors the workflow create body (snake_case) so a published
// workflow template is exactly what the builder would POST.
type workflowPayload struct {
	Name          string                            `json:"name"`
	IsActive      bool                              `json:"is_active"`
	TriggerType   string                            `json:"trigger_type"`
	TriggerConfig map[string]interface{}            `json:"trigger_config"`
	BotName       string                            `json:"bot_name"`
	ChannelID     string                            `json:"channel_id"`
	Keywords      []string                          `json:"keywords"`
	MatchType     string                            `json:"match_type"`
	Actions       []workflowBusiness.WorkflowAction `json:"actions"`
}

func installWorkflow(ctx context.Context, payload string, userID uuid.UUID) (*InstallResult, error) {
	var p workflowPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, fmt.Errorf("invalid workflow template")
	}
	in := workflowBusiness.WorkflowInput{
		Name:          p.Name,
		IsActive:      false, // installed inactive; the installer enables it
		TriggerType:   p.TriggerType,
		TriggerConfig: p.TriggerConfig,
		BotName:       p.BotName,
		ChannelID:     "", // channel ids are workspace-specific; start unbound
		Keywords:      p.Keywords,
		MatchType:     p.MatchType,
		Actions:       p.Actions,
	}
	wf, err := workflowBusiness.CreateWorkflow(ctx, in, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to install workflow: %w", err)
	}
	return &InstallResult{Kind: model.KindWorkflow, EntityID: wf.Id.String(), Name: wf.Name}, nil
}

// ── table ──

// tablePayload captures a table's structure (header + columns + saved views).
type tablePayload struct {
	Table  dataTableBusiness.TableInput   `json:"table"`
	Fields []dataTableBusiness.FieldInput `json:"fields"`
	Views  []dataTableBusiness.ViewInput  `json:"views"`
}

func installTable(ctx context.Context, payload string, userInfo *userModels.UserInfo) (*InstallResult, error) {
	var p tablePayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, fmt.Errorf("invalid table template")
	}
	actor := dataTableBusiness.Actor{UserID: userInfo.UserPostgresInfo.Id, IsAdmin: userInfo.UserPostgresInfo.IsAdmin}
	if strings.TrimSpace(p.Table.Name) == "" {
		return nil, fmt.Errorf("table template is missing a name")
	}
	tbl, err := dataTableBusiness.CreateTableFromTemplate(ctx, p.Table, p.Fields, p.Views, actor)
	if err != nil {
		return nil, fmt.Errorf("failed to install table: %w", err)
	}
	return &InstallResult{Kind: model.KindTable, EntityID: tbl.Id.String(), Name: tbl.Name}, nil
}
