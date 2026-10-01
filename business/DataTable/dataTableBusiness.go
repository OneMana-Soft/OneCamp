// Package business (DataTable) is the management + access layer for Tables: a
// first-class, Notion-style structured-data entity. It validates input,
// enforces the permission model, and broadcasts row changes over MQTT for live
// collaboration.
//
// Permission model (see migration 91):
//   - view:        admin, owner, or any member if the table is 'workspace'.
//   - edit rows:   same as view (a 'workspace' table is collaboratively edited;
//     a 'private' table is owner/admin only).
//   - manage:      admin or owner only — structure (fields/views), table
//     settings, and deletion.
package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

const (
	maxNameLen        = 120
	maxDescriptionLen = 2000
	maxIconLen        = 64
	maxValuesBytes    = 100 * 1024 // 100 KiB per row values blob
)

// Actor is the user performing an action.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
}

var (
	errForbidden = fmt.Errorf("not authorized for this table")
	errNotFound  = fmt.Errorf("table not found")
)

// IsForbidden / IsNotFound let controllers map to HTTP codes.
func IsForbidden(err error) bool { return err == errForbidden }
func IsNotFound(err error) bool  { return err == errNotFound }

func canView(t *model.DataTable, a Actor) bool {
	return a.IsAdmin || t.CreatedBy == a.UserID || t.Visibility == model.VisibilityWorkspace
}

func canManage(t *model.DataTable, a Actor) bool {
	return a.IsAdmin || t.CreatedBy == a.UserID
}

// Inputs from the controller.
type TableInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Visibility  string `json:"visibility"`
}

type FieldInput struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	Config   map[string]interface{} `json:"config"`
	Position float64                `json:"position"`
}

type RowInput struct {
	Values   map[string]interface{} `json:"values"`
	Position float64                `json:"position"`
}

type ViewInput struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	Config   map[string]interface{} `json:"config"`
	Position float64                `json:"position"`
}

// TableBundle is the one-call payload for opening a table: header + structure +
// the first page of rows, so the FE renders without a request waterfall.
type TableBundle struct {
	Table     *model.DataTable `json:"table"`
	Fields    []*model.Field   `json:"fields"`
	Views     []*model.View    `json:"views"`
	Rows      []*model.Row     `json:"rows"`
	CanManage bool             `json:"can_manage"`
	MqttTopic string           `json:"mqtt_topic"`
	// RowsTruncated is true when the table has more rows than the bundle's
	// first page (bundleRowPage). The FE/AI should page the rest via the rows
	// endpoint rather than assume Rows is the whole table.
	RowsTruncated bool `json:"rows_truncated"`
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func jsonObject(m map[string]interface{}) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ───────────────────────── tables ─────────────────────────

// CreateTable validates and creates a table, seeding a default "Name" text
// field and a default grid view so it is immediately usable.
func CreateTable(ctx context.Context, in TableInput, actor Actor) (*model.DataTable, error) {
	id, err := createTableRow(ctx, in, actor)
	if err != nil {
		return nil, err
	}
	// Seed a default "Name" text column and a grid view (best-effort).
	seedDefaults(ctx, id)
	return model.GetTableByID(ctx, id)
}

// CreateTableFromTemplate creates a table and its columns/views from a template
// payload, WITHOUT seeding the usual defaults (the template supplies its own
// structure, so seeding would duplicate the "Name" column / "Grid" view). If
// the template carries no fields or no views, the matching default is seeded so
// the table is never left unusable. Best-effort on each child so one bad column
// does not abort the install.
func CreateTableFromTemplate(ctx context.Context, in TableInput, fields []FieldInput, views []ViewInput, actor Actor) (*model.DataTable, error) {
	id, err := createTableRow(ctx, in, actor)
	if err != nil {
		return nil, err
	}
	addedField := false
	for _, fi := range fields {
		if strings.TrimSpace(fi.Name) == "" {
			continue
		}
		f, ferr := buildField(id, fi)
		if ferr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate field err: %v", ferr)
			continue
		}
		if _, cerr := model.CreateField(ctx, f); cerr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate create field err: %v", cerr)
			continue
		}
		addedField = true
	}
	addedView := false
	for _, vi := range views {
		v, verr := buildView(id, vi)
		if verr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate view err: %v", verr)
			continue
		}
		if _, cerr := model.CreateView(ctx, v); cerr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate create view err: %v", cerr)
			continue
		}
		addedView = true
	}
	if !addedField {
		seedNameField(ctx, id)
	}
	if !addedView {
		seedGridView(ctx, id)
	}
	return model.GetTableByID(ctx, id)
}

// createTableRow validates a TableInput and inserts the table row (no seeding),
// returning the new id.
func createTableRow(ctx context.Context, in TableInput, actor Actor) (uuid.UUID, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return uuid.Nil, fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen {
		return uuid.Nil, fmt.Errorf("name is too long")
	}
	if len(in.Description) > maxDescriptionLen {
		return uuid.Nil, fmt.Errorf("description is too long")
	}
	if len(in.Icon) > maxIconLen {
		return uuid.Nil, fmt.Errorf("icon is too long")
	}
	visibility := strings.TrimSpace(in.Visibility)
	if visibility == "" {
		visibility = model.VisibilityWorkspace
	}
	if !model.ValidVisibility(visibility) {
		return uuid.Nil, fmt.Errorf("invalid visibility")
	}
	t := &model.DataTable{
		Name:        name,
		Description: optStr(in.Description),
		Icon:        optStr(in.Icon),
		Visibility:  visibility,
		CreatedBy:   actor.UserID,
	}
	id, err := model.CreateTable(ctx, t)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to create table")
	}
	return id, nil
}

// seedDefaults adds a default "Name" text column and a grid view (best-effort).
func seedDefaults(ctx context.Context, id uuid.UUID) {
	seedNameField(ctx, id)
	seedGridView(ctx, id)
}

// seedNameField adds the default "Name" text column (best-effort).
func seedNameField(ctx context.Context, id uuid.UUID) {
	if _, ferr := model.CreateField(ctx, &model.Field{TableId: id, Name: "Name", Type: model.FieldText, Config: "{}", Position: 0}); ferr != nil {
		helpers.LogErrorWithContext(ctx, "seedNameField err: %v", ferr)
	}
}

// seedGridView adds the default grid view (best-effort).
func seedGridView(ctx context.Context, id uuid.UUID) {
	if _, verr := model.CreateView(ctx, &model.View{TableId: id, Name: "Grid", Type: model.ViewGrid, Config: "{}", Position: 0}); verr != nil {
		helpers.LogErrorWithContext(ctx, "seedGridView err: %v", verr)
	}
}

// loadManageable loads a table the actor may manage, or returns the typed error.
func loadManageable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataTable, error) {
	t, err := model.GetTableByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	if t == nil {
		return nil, errNotFound
	}
	if !canManage(t, actor) {
		return nil, errForbidden
	}
	return t, nil
}

// loadViewable loads a table the actor may view, or returns the typed error.
// CanView answers "may this actor see this table" and nothing else.
//
// Exists so an authorization layer can ask the question without paying for the
// answer to a different one. GetBundle also enforces viewability, but it then
// fetches fields, views and rows concurrently — three round-trips a caller that
// only wants a yes or no does not need, and that a caller which is about to be
// refused should certainly not pay for.
//
// Returns the same sentinels as every other path here, so IsForbidden and IsNotFound
// classify it identically. Liveness needs no separate check: GetTableByID filters
// `deleted_at IS NULL`, so a soft-deleted table is indistinguishable from one that
// never existed, which is the correct answer to give an agent either way.
func CanView(ctx context.Context, id uuid.UUID, actor Actor) error {
	_, err := loadViewable(ctx, id, actor)
	return err
}

// CanManage answers "may this actor CHANGE this table" — a strictly stronger claim
// than CanView, since a workspace-visible table is readable by everyone and
// modifiable only by its creator or an admin.
//
// Same signature as CanView on purpose, so an authorization layer can select between
// them by intent rather than branching into two differently shaped calls.
func CanManage(ctx context.Context, id uuid.UUID, actor Actor) error {
	t, err := loadViewable(ctx, id, actor)
	if err != nil {
		return err
	}
	if !canManage(t, actor) {
		return errForbidden
	}
	return nil
}

func loadViewable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataTable, error) {
	t, err := model.GetTableByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	if t == nil {
		return nil, errNotFound
	}
	if !canView(t, actor) {
		return nil, errForbidden
	}
	return t, nil
}

// UpdateTable updates a table's header (manage only).
func UpdateTable(ctx context.Context, id uuid.UUID, in TableInput, actor Actor) (*model.DataTable, error) {
	t, err := loadManageable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen || len(in.Description) > maxDescriptionLen || len(in.Icon) > maxIconLen {
		return nil, fmt.Errorf("a field is too long")
	}
	visibility := strings.TrimSpace(in.Visibility)
	if visibility == "" {
		visibility = t.Visibility
	}
	if !model.ValidVisibility(visibility) {
		return nil, fmt.Errorf("invalid visibility")
	}
	t.Name = name
	t.Description = optStr(in.Description)
	t.Icon = optStr(in.Icon)
	t.Visibility = visibility
	if err := model.UpdateTable(ctx, t); err != nil {
		return nil, fmt.Errorf("failed to update table")
	}
	return model.GetTableByID(ctx, id)
}

// DeleteTable soft-deletes a table (manage only).
func DeleteTable(ctx context.Context, id uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, id, actor); err != nil {
		return err
	}
	return model.SoftDeleteTable(ctx, id)
}

// ListTables returns the tables the actor may see.
func ListTables(ctx context.Context, actor Actor) ([]*model.DataTable, error) {
	return model.ListTablesVisibleTo(ctx, actor.UserID, actor.IsAdmin)
}

// GetBundle returns the full open-payload for a table the actor may view.
func GetBundle(ctx context.Context, id uuid.UUID, actor Actor) (*TableBundle, error) {
	t, err := loadViewable(ctx, id, actor)
	if err != nil {
		return nil, err
	}

	// Fields, views, and rows are mutually independent (each only scopes by
	// table_id) and back the single most-hit table read (the app's table view,
	// GET /v1/tables/{id}, and the AI read_table tool). Fetch them concurrently
	// so the bundle is one round-trip's wall-clock latency instead of three
	// serial ones. database/sql's pool is safe for concurrent use.
	var (
		fields []*model.Field
		views  []*model.View
		rows   []*model.Row
		fErr   error
		vErr   error
		rErr   error
		wg     sync.WaitGroup
	)
	// Fetch one more than the page size so we can tell the FE/AI whether the
	// table has more rows than this bundle carries, without a separate COUNT.
	const bundleRowPage = 500
	wg.Add(3)
	go func() { defer wg.Done(); fields, fErr = model.ListFields(ctx, id) }()
	go func() { defer wg.Done(); views, vErr = model.ListViews(ctx, id) }()
	go func() { defer wg.Done(); rows, rErr = model.ListRows(ctx, id, bundleRowPage+1, 0) }()
	wg.Wait()

	if fErr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if vErr != nil {
		return nil, fmt.Errorf("failed to load views")
	}
	if rErr != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	truncated := false
	if len(rows) > bundleRowPage {
		rows = rows[:bundleRowPage]
		truncated = true
	}
	return &TableBundle{Table: t, Fields: fields, Views: views, Rows: rows, CanManage: canManage(t, actor), MqttTopic: helpers.GetMqttTopicForTable(t.Id.String()), RowsTruncated: truncated}, nil
}

// GetGuestBundle returns the open-payload for a table for an EXTERNAL guest,
// authorized by a share-link grant (the caller validates the grant). Unlike
// GetBundle it performs NO member-actor access check — the grant is the
// authorization — and returns a strictly read-only bundle (CanManage false and
// no live MQTT topic, so the guest UI never attempts a member-only subscription
// or write).
func GetGuestBundle(ctx context.Context, id uuid.UUID) (*TableBundle, error) {
	t, err := model.GetTableByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	if t == nil {
		return nil, fmt.Errorf("table not found")
	}

	var (
		fields []*model.Field
		views  []*model.View
		rows   []*model.Row
		fErr   error
		vErr   error
		rErr   error
		wg     sync.WaitGroup
	)
	const bundleRowPage = 500
	wg.Add(3)
	go func() { defer wg.Done(); fields, fErr = model.ListFields(ctx, id) }()
	go func() { defer wg.Done(); views, vErr = model.ListViews(ctx, id) }()
	go func() { defer wg.Done(); rows, rErr = model.ListRows(ctx, id, bundleRowPage+1, 0) }()
	wg.Wait()

	if fErr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if vErr != nil {
		return nil, fmt.Errorf("failed to load views")
	}
	if rErr != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	truncated := false
	if len(rows) > bundleRowPage {
		rows = rows[:bundleRowPage]
		truncated = true
	}
	return &TableBundle{Table: t, Fields: fields, Views: views, Rows: rows, CanManage: false, MqttTopic: "", RowsTruncated: truncated}, nil
}

// ───────────────────────── fields ─────────────────────────

// CreateField adds a column (manage only).
func CreateField(ctx context.Context, tableId uuid.UUID, in FieldInput, actor Actor) (*model.Field, error) {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	f, err := buildField(tableId, in)
	if err != nil {
		return nil, err
	}
	id, err := model.CreateField(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("failed to create field")
	}
	f.Id = id
	return f, nil
}

// UpdateField edits a column (manage only).
func UpdateField(ctx context.Context, tableId, fieldId uuid.UUID, in FieldInput, actor Actor) error {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return err
	}
	f, err := buildField(tableId, in)
	if err != nil {
		return err
	}
	f.Id = fieldId
	if err := model.UpdateField(ctx, f); err != nil {
		return fmt.Errorf("failed to update field")
	}
	return nil
}

// DeleteField removes a column (manage only).
func DeleteField(ctx context.Context, tableId, fieldId uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return err
	}
	return model.DeleteField(ctx, tableId, fieldId)
}

func buildField(tableId uuid.UUID, in FieldInput) (*model.Field, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("field name is required")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("field name is too long")
	}
	ftype := strings.TrimSpace(in.Type)
	if ftype == "" {
		ftype = model.FieldText
	}
	if !model.ValidFieldType(ftype) {
		return nil, fmt.Errorf("invalid field type")
	}
	cfg, err := jsonObject(in.Config)
	if err != nil {
		return nil, fmt.Errorf("invalid field config")
	}
	return &model.Field{TableId: tableId, Name: name, Type: ftype, Config: cfg, Position: in.Position}, nil
}

// ───────────────────────── rows ─────────────────────────

// CreateRow inserts a row (view/edit access) and broadcasts it.
func CreateRow(ctx context.Context, tableId uuid.UUID, in RowInput, actor Actor) (*model.Row, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	valuesJSON, verr := validateValues(in.Values)
	if verr != nil {
		return nil, verr
	}
	r := &model.Row{TableId: tableId, Values: valuesJSON, Position: in.Position, CreatedBy: &actor.UserID}
	created, cerr := model.CreateRow(ctx, r)
	if cerr != nil {
		return nil, fmt.Errorf("failed to create row")
	}
	broadcastRow(t.Id.String(), "created", created)
	// Continuous AI autofill: recompute any auto AI columns for the new row,
	// as the creator, off the request path. Loop-safe (writes via the model
	// layer, not CreateRow/UpdateRow).
	go AutofillRowOnWrite(context.WithoutCancel(ctx), t, created, actor)
	// Reactive triggers: let agents react to new table rows (Notion-DB-style
	// automation). Loop-safe — DispatchEvent skips in-process listeners for
	// automation-generated writes, so an agent's own row write can't re-trigger.
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "table.row.created", map[string]interface{}{
		"table_id":   t.Id.String(),
		"table_name": t.Name,
		"row_id":     created.Id.String(),
	})
	return created, nil
}

// UpdateRow replaces a row's values (view/edit access) and broadcasts it.
func UpdateRow(ctx context.Context, tableId, rowId uuid.UUID, in RowInput, actor Actor) (*model.Row, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	valuesJSON, verr := validateValues(in.Values)
	if verr != nil {
		return nil, verr
	}
	r, uerr := model.UpdateRowValues(ctx, tableId, rowId, valuesJSON, in.Position)
	if uerr != nil {
		return nil, fmt.Errorf("failed to update row")
	}
	broadcastRow(t.Id.String(), "updated", r)
	// Continuous AI autofill: recompute any auto AI columns so derived cells
	// track the row's latest inputs. Loop-safe (model-layer write, no event).
	go AutofillRowOnWrite(context.WithoutCancel(ctx), t, r, actor)
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "table.row.updated", map[string]interface{}{
		"table_id":   t.Id.String(),
		"table_name": t.Name,
		"row_id":     r.Id.String(),
	})
	return r, nil
}

// DeleteRow soft-deletes a row (view/edit access) and broadcasts it.
func DeleteRow(ctx context.Context, tableId, rowId uuid.UUID, actor Actor) error {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return err
	}
	if err := model.DeleteRow(ctx, tableId, rowId); err != nil {
		return fmt.Errorf("failed to delete row")
	}
	mqttBusiness.PublishTableRow(t.Id.String(), map[string]interface{}{"action": "deleted", "row_id": rowId.String()})
	return nil
}

// ListRows returns a page of rows for a table the actor may view.
func ListRows(ctx context.Context, tableId uuid.UUID, actor Actor, limit, offset int) ([]*model.Row, error) {
	if _, err := loadViewable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	return model.ListRows(ctx, tableId, limit, offset)
}

// ListRowsFiltered returns a page of rows for a table the actor may view whose
// raw JSONB text contains every one of likeSubstrings (case-insensitive). It is
// the permission-checked wrapper the aggregation engine uses to push a coarse,
// superset row filter down to the database (see model.ListRowsFiltered): the
// exact typed predicates are still re-applied in memory, so it only reduces how
// many rows are scanned, never the result.
func ListRowsFiltered(ctx context.Context, tableId uuid.UUID, actor Actor, likeSubstrings []string, limit, offset int) ([]*model.Row, error) {
	if _, err := loadViewable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	return model.ListRowsFiltered(ctx, tableId, likeSubstrings, limit, offset)
}

// validateValues ensures the row values are a JSON object within the size cap
// and returns the encoded blob.
func validateValues(values map[string]interface{}) (string, error) {
	if values == nil {
		return "{}", nil
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("invalid row values")
	}
	if len(b) > maxValuesBytes {
		return "", fmt.Errorf("row is too large")
	}
	return string(b), nil
}

// broadcastRow publishes a created/updated row to the table's live topic.
func broadcastRow(tableID, action string, r *model.Row) {
	if r == nil {
		return
	}
	mqttBusiness.PublishTableRow(tableID, map[string]interface{}{"action": action, "row_id": r.Id.String(), "row": r})
}

// ───────────────────────── views ─────────────────────────

// CreateView adds a saved view (manage only).
func CreateView(ctx context.Context, tableId uuid.UUID, in ViewInput, actor Actor) (*model.View, error) {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	v, err := buildView(tableId, in)
	if err != nil {
		return nil, err
	}
	id, err := model.CreateView(ctx, v)
	if err != nil {
		return nil, fmt.Errorf("failed to create view")
	}
	v.Id = id
	return v, nil
}

// UpdateView edits a saved view (manage only).
func UpdateView(ctx context.Context, tableId, viewId uuid.UUID, in ViewInput, actor Actor) error {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return err
	}
	v, err := buildView(tableId, in)
	if err != nil {
		return err
	}
	v.Id = viewId
	if err := model.UpdateView(ctx, v); err != nil {
		return fmt.Errorf("failed to update view")
	}
	return nil
}

// DeleteView removes a saved view (manage only).
func DeleteView(ctx context.Context, tableId, viewId uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return err
	}
	return model.DeleteView(ctx, tableId, viewId)
}

func buildView(tableId uuid.UUID, in ViewInput) (*model.View, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "View"
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("view name is too long")
	}
	vtype := strings.TrimSpace(in.Type)
	if vtype == "" {
		vtype = model.ViewGrid
	}
	if !model.ValidViewType(vtype) {
		return nil, fmt.Errorf("invalid view type")
	}
	cfg, err := jsonObject(in.Config)
	if err != nil {
		return nil, fmt.Errorf("invalid view config")
	}
	return &model.View{TableId: tableId, Name: name, Type: vtype, Config: cfg, Position: in.Position}, nil
}

// AutofillRowOnWrite is a no-op on the AI-free edition. AI columns do not exist here.
func AutofillRowOnWrite(ctx context.Context, t *model.DataTable, row *model.Row, actor Actor) {}
