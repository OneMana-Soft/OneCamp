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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
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

// tooLong is whether a name or a description is over max characters. People
// count characters, and the web's fields do: counted in bytes, a name in
// Hindi or Tamil (three bytes a letter) was refused at 41 letters of 120.
func tooLong(s string, max int) bool { return utf8.RuneCountInString(s) > max }

// Actor is the user performing an action.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
	// Kind is how they act: themselves in the app (the zero value), or
	// through the API, an AI tool, an import, a form. It decides whether a
	// new row must give every required field (completeWrite).
	Kind rowLedger.ActorKind
}

// completeWrite is whether a's new rows must give every required field: one
// made through the API, by an AI tool or by a form, never the grid's blank
// new row or an import of what another tool held.
func completeWrite(a Actor) bool {
	switch a.Kind {
	case rowLedger.ActorAPI, rowLedger.ActorAgent, rowLedger.ActorForm:
		return true
	}
	return false
}

var (
	errForbidden   = fmt.Errorf("not authorized for this table")
	errNotFound    = fmt.Errorf("table not found")
	errRowNotFound = fmt.Errorf("row not found")
	// errFieldNotFound is a field that isn't in the table, or no longer is.
	errFieldNotFound = fmt.Errorf("that field isn't in this table")
	// errFieldChanged is a field whose type changed while a save was planned
	// against the type it had.
	errFieldChanged = fmt.Errorf("this field changed while you were saving it; try again")
	// errNoCells is a patch naming no cell a row stores: it would touch the
	// row, and tell its readers, webhooks and AI columns, for nothing.
	errNoCells = fmt.Errorf("name the cells to change")
	// errNumberSigns is number signs no number is written with
	// (NumberSigns.Checked).
	errNumberSigns = fmt.Errorf("invalid number signs: a decimal sign and a group sign are one character each, and differ")
	// errRowsUnavailable is a page of rows that couldn't be read, or worked
	// out for its reader, just now: no row goes out as it's stored.
	errRowsUnavailable = fmt.Errorf("couldn't load the rows")
)

// IsForbidden / IsNotFound / IsRowNotFound / IsUnavailable let controllers map
// to HTTP codes.
func IsForbidden(err error) bool   { return err == errForbidden }
func IsNotFound(err error) bool    { return err == errNotFound }
func IsRowNotFound(err error) bool { return err == errRowNotFound }
func IsUnavailable(err error) bool { return err == errRowsUnavailable }

func canView(t *model.DataTable, a Actor) bool {
	return t.Visibility == model.VisibilityWorkspace || helpers.OwnerOrAdmin(&t.CreatedBy, a.UserID, a.IsAdmin)
}

// canManage is helpers.OwnerOrAdmin: a table with no recorded creator is the
// admins' alone, and nobody signed in is never its creator.
func canManage(t *model.DataTable, a Actor) bool {
	return helpers.OwnerOrAdmin(&t.CreatedBy, a.UserID, a.IsAdmin)
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
	// Description says what the field is for.
	Description string `json:"description"`
	// Required is a field every row made through the API, an AI tool or a
	// form must give.
	Required bool `json:"required"`
	// Default is what a new row's cell starts as; JSON null or nothing is
	// none.
	Default json.RawMessage `json:"default_value"`
	// keptDefault is a default the save didn't name, the field's own: one
	// its new type can't read goes rather than refusing the save.
	keptDefault bool
}

// FieldPatch is a change to a field: what it names changes, and what it
// leaves out keeps its stored value (onto). A save sent its page's copy of
// the field whole, so a rename put back the options, settings and place the
// copy held, and one that sent no place moved the column to the front.
type FieldPatch struct {
	Name *string `json:"name"`
	Type *string `json:"type"`
	// Config is left out by a nil map: JSON null, or no "config" at all.
	Config   map[string]interface{} `json:"config"`
	Position *float64               `json:"position"`
	// Description, Required and Default change those settings; left out,
	// they keep theirs. A JSON null default takes the default away.
	Description *string         `json:"description"`
	Required    *bool           `json:"required"`
	Default     json.RawMessage `json:"default_value"`
	// Primary true makes the field the one that names the table's rows, and
	// its first column. A field stops being primary only by another becoming it.
	Primary *bool `json:"primary"`
	// NumberSigns are how whoever changes the field's type writes numbers
	// ("decimal" and "group"): text in a column made numbers is read by them.
	formula.NumberSigns
}

// onto is field f as a save of p leaves it: what p names, and f's stored
// value of the rest. An empty type is one left out.
func (p FieldPatch) onto(f *model.Field) FieldInput {
	in := FieldInput{Name: f.Name, Type: f.Type, Config: p.Config, Position: f.Position, Required: f.Required, Default: f.Default, keptDefault: true}
	if f.Description != nil {
		in.Description = *f.Description
	}
	if p.Description != nil {
		in.Description = *p.Description
	}
	if p.Required != nil {
		in.Required = *p.Required
	}
	if p.Default != nil {
		in.Default, in.keptDefault = p.Default, false
	}
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Type != nil && strings.TrimSpace(*p.Type) != "" {
		in.Type = *p.Type
	}
	if p.Position != nil {
		in.Position = *p.Position
	}
	if in.Config == nil {
		in.Config = map[string]interface{}{}
		_ = json.Unmarshal([]byte(f.Config), &in.Config)
	}
	if p.Default == nil && in.Type != f.Type {
		in.Default = convertedDefault(f.Default, in.Type)
	}
	return in
}

// convertedDefault is a field's default as its new kind reads it, as its
// cells are converted (nil when it can't be one): a default a type change
// leaves unreadable goes, rather than refusing the change. Pure.
func convertedDefault(raw json.RawMessage, to string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	out, _, _ := fieldTypes.ConvertCell(to, v, formula.PlainSigns)
	if out == nil {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// whole is whether p is a save of a whole field: its name, type and place
// all named. The web released before patches (v2.72.2's) sends every save of
// a field that way, from its page's copy, and the web since never does (it
// names what changed). Such a save is read as v2.72.2 read it, so a page
// still running that web changes no cell it didn't: the options it lists are
// the field's (planOptions), and a change of type leaves the cells as they
// are.
func (p FieldPatch) whole() bool { return p.Name != nil && p.Type != nil && p.Position != nil }

type RowInput struct {
	Values map[string]interface{} `json:"values"`
	// Position is the row's place; left out, a new row goes after the last
	// one and an updated row stays where it is.
	Position *float64 `json:"position"`
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
	// RowCount is how many rows the table has, not deleted: what a reader
	// holding the first page says it holds of, and pages towards.
	RowCount int `json:"row_count"`
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
	add := func(fi FieldInput) *model.Field {
		if target, _ := fi.Config["relation_target"].(string); target == linksTable || strings.TrimSpace(fi.Type) == model.FieldRollup {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate skips %q: it links to another table", fi.Name)
			return nil
		}
		f, _, ferr := buildField(id, fi, nil, false)
		if ferr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate field err: %v", ferr)
			return nil
		}
		fid, cerr := model.CreateField(ctx, f)
		if cerr != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate create field err: %v", cerr)
			return nil
		}
		f.Id = fid
		addedField = true
		return f
	}
	// Formulas come last, so the fields they read exist, and they're stored
	// with those fields by id, as a formula added by hand is.
	var formulas []FieldInput
	for _, fi := range fields {
		if strings.TrimSpace(fi.Name) == "" {
			continue
		}
		if strings.TrimSpace(fi.Type) == model.FieldFormula {
			formulas = append(formulas, fi)
			continue
		}
		add(fi)
	}
	// Each formula after the formulas it reads, checked against the fields
	// made so far: loaded once, and added to as each is made.
	if len(formulas) > 0 {
		made, err := model.ListFields(ctx, id)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate load fields err: %v", err)
			formulas = nil
		}
		try := func(fi FieldInput) error {
			cfg, err := formulaConfigWith(made, uuid.Nil, fi.Config)
			if err != nil {
				return err
			}
			fi.Config = cfg
			if f := add(fi); f != nil {
				made = append(made, f)
			}
			return nil
		}
		// One more pass for those that failed: of two formulas with one
		// name, one can read a field made after it.
		var failed []FieldInput
		for _, fi := range formulasInOrder(formulas) {
			if try(fi) != nil {
				failed = append(failed, fi)
			}
		}
		for _, fi := range failed {
			if err := try(fi); err != nil {
				helpers.LogErrorWithContext(ctx, "CreateTableFromTemplate formula %q err: %v", fi.Name, err)
			}
		}
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
	if tooLong(name, maxNameLen) {
		return uuid.Nil, fmt.Errorf("name is too long")
	}
	if tooLong(in.Description, maxDescriptionLen) {
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

// ViewableBy is CanView as a yes or no: no for a table the actor may not see
// or that doesn't exist, and an error only when it couldn't be decided.
func ViewableBy(ctx context.Context, actor Actor, id uuid.UUID) (bool, error) {
	err := CanView(ctx, id, actor)
	if IsForbidden(err) || IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
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
	if !opens(ctx, t, actor) {
		return nil, errForbidden
	}
	return t, nil
}

// TablePatch is a change to a table's header: only the fields it names
// change.
type TablePatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Icon        *string `json:"icon"`
	Visibility  *string `json:"visibility"`
}

// UpdateTable changes the header fields a patch names, and only those (manage
// only), and has the table's open views read it again. The header used to be
// written whole from the page's copy, so a rename from a copy read before the
// table was made private made it visible to the workspace again (TAB-BUG-12).
// Who can see a table changes on its own; a whole header, as the web released
// before patches sends every header save, is read for the one change it makes
// (wholeHeader). It returns the table, and its visibility as it was.
func UpdateTable(ctx context.Context, id uuid.UUID, in TablePatch, actor Actor) (*model.DataTable, string, error) {
	t, err := loadManageable(ctx, id, actor)
	if err != nil {
		return nil, "", err
	}
	if in.Name == nil && in.Description == nil && in.Icon == nil && in.Visibility == nil {
		return nil, "", fmt.Errorf("name what to change: name, description, icon or visibility")
	}
	if in.Name != nil && in.Visibility != nil {
		if in, err = wholeHeader(t, in); err != nil {
			return nil, "", err
		}
	}
	if in.Visibility != nil && (in.Name != nil || in.Description != nil || in.Icon != nil) {
		return nil, "", fmt.Errorf("change who can see a table on its own")
	}
	change := model.TableChange{SetDescription: in.Description != nil, SetIcon: in.Icon != nil}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, "", fmt.Errorf("name is required")
		}
		if tooLong(name, maxNameLen) {
			return nil, "", fmt.Errorf("name is too long")
		}
		change.Name = &name
	}
	if in.Description != nil {
		if tooLong(*in.Description, maxDescriptionLen) {
			return nil, "", fmt.Errorf("description is too long")
		}
		change.Description = optStr(*in.Description)
	}
	if in.Icon != nil {
		if len(*in.Icon) > maxIconLen {
			return nil, "", fmt.Errorf("icon is too long")
		}
		change.Icon = optStr(*in.Icon)
	}
	if in.Visibility != nil {
		visibility := strings.TrimSpace(*in.Visibility)
		if !model.ValidVisibility(visibility) {
			return nil, "", fmt.Errorf("invalid visibility")
		}
		change.Visibility = &visibility
	}
	t, was, err := model.UpdateTable(ctx, id, change)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", errNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to update table")
	}
	mqttBusiness.PublishTableRow(id.String(), map[string]interface{}{"action": "table"})
	if change.Visibility != nil && *change.Visibility != was {
		// Who may open its rows' pages changed with it (rowPages.go).
		tablePagesChanged(ctx, id)
	}
	return t, was, nil
}

// errStaleHeader is a whole header (wholeHeader) that differs from the table
// in more than one way: a page's copy read before the table changed.
var errStaleHeader = fmt.Errorf("this table changed since the page was opened; reload it and try again")

// wholeHeader reads a header patch naming both the name and who can see the
// table, as the web released before patches (v2.72.2's) sends every header
// save, and the web since never does: the page's copy of the name, of the
// description and icon when it has them, and of the visibility, with one of
// them changed. That one is the save, found against the table as stored (t):
//   - a rename, or a description, which that page leaves out when it's empty,
//     so one left out is cleared;
//   - who can see the table (Make private, Show to the workspace), alone;
//   - nothing, when the copy matches the table.
//
// The icon is the copy's, as that page never changes it. A copy that differs
// in more than one way was read before the table changed, and would put back
// what changed since, a private table shown to the workspace among it
// (TAB-BUG-12): it's refused.
func wholeHeader(t *model.DataTable, in TablePatch) (TablePatch, error) {
	description := ""
	if in.Description != nil {
		description = strings.TrimSpace(*in.Description)
	}
	stored := ""
	if t.Description != nil {
		stored = strings.TrimSpace(*t.Description)
	}
	renamed := strings.TrimSpace(*in.Name) != t.Name
	described := description != stored
	shown := strings.TrimSpace(*in.Visibility) != t.Visibility
	switch {
	case (renamed && described) || (renamed && shown) || (described && shown):
		return TablePatch{}, errStaleHeader
	case described:
		return TablePatch{Description: &description}, nil
	case shown:
		return TablePatch{Visibility: in.Visibility}, nil
	}
	return TablePatch{Name: in.Name}, nil
}

// DeleteTable soft-deletes a table (manage only): it goes to the tables'
// trash, from which its creator or an admin can bring it back (RestoreTable).
func DeleteTable(ctx context.Context, id uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, id, actor); err != nil {
		return err
	}
	if err := model.SoftDeleteTable(ctx, id, actor.UserID); err != nil {
		return err
	}
	// Its rows' pages go to the trash with it (rowPages.go).
	tablePagesChanged(ctx, id)
	return nil
}

// ListTables returns the tables the actor may see.
func ListTables(ctx context.Context, actor Actor) ([]*model.DataTable, error) {
	return model.ListTablesVisibleTo(ctx, actor.UserID, actor.IsAdmin)
}

// GetBundle returns the full open-payload for a table the actor may view.
func GetBundle(ctx context.Context, id uuid.UUID, actor Actor) (*TableBundle, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, id, actor)
	if err != nil {
		return nil, err
	}

	return loadBundle(ctx, t, canManage(t, actor), helpers.GetMqttTopicForTable(t.Id.String()))
}

// bundleRowPage is how many rows a bundle carries; more are paged with the
// rows endpoint.
const bundleRowPage = 500

// loadBundle reads a table's fields, views and first page of rows, with its
// formulas worked out: the one loader behind the member's and the guest's
// bundle.
func loadBundle(ctx context.Context, t *model.DataTable, manage bool, topic string) (*TableBundle, error) {
	// Fields, views, rows and the row count are mutually independent (each only
	// scopes by table_id) and back the single most-hit table read (the app's
	// table view, GET /v1/tables/{id}, and the AI read_table tool). Fetch them
	// concurrently so the bundle is one round-trip's wall-clock latency instead
	// of four serial ones. database/sql's pool is safe for concurrent use.
	var (
		fields []*model.Field
		views  []*model.View
		rows   []*model.Row
		count  int
		fErr   error
		vErr   error
		rErr   error
		cErr   error
		wg     sync.WaitGroup
	)
	// Fetch one more than the page size so we can tell the FE/AI whether the
	// table has more rows than this bundle carries, by the page itself.
	wg.Add(4)
	helpers.GoSafeNamed("business/DataTable/dataTableBusiness:loadBundle", func() { defer wg.Done(); fields, fErr = model.ListFields(ctx, t.Id) })
	helpers.GoSafeNamed("business/DataTable/dataTableBusiness:loadBundle", func() { defer wg.Done(); views, vErr = model.ListViews(ctx, t.Id) })
	helpers.GoSafeNamed("business/DataTable/dataTableBusiness:loadBundle", func() { defer wg.Done(); rows, rErr = model.ListRows(ctx, t.Id, bundleRowPage+1, 0) })
	helpers.GoSafeNamed("business/DataTable/dataTableBusiness:loadBundle", func() { defer wg.Done(); count, cErr = model.CountRows(ctx, t.Id) })
	wg.Wait()

	if fErr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if vErr != nil {
		return nil, fmt.Errorf("failed to load views")
	}
	if rErr != nil || cErr != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	truncated := false
	if len(rows) > bundleRowPage {
		rows = rows[:bundleRowPage]
		truncated = true
	}
	presentComputedFields(fields, withComputed(ctx, fields, rows))
	return &TableBundle{Table: t, Fields: fields, Views: views, Rows: rows, CanManage: manage, MqttTopic: topic,
		RowsTruncated: truncated, RowCount: count}, nil
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
		return nil, errNotFound // gone, unlike a load that failed (IsNotFound)
	}

	b, err := loadBundle(ctx, t, false, "")
	if err != nil {
		return nil, err
	}
	forGuests(b)
	return b, nil
}

// forGuests takes out of a bundle what a guest has no use for, and members
// do: who made the table and each row, which are member ids; the saved views,
// whose filters name what members look for; and every field setting a
// guest's view doesn't read (guestConfig).
func forGuests(b *TableBundle) {
	b.Table.CreatedBy = uuid.Nil
	b.Views = []*model.View{}
	for _, r := range b.Rows {
		// Who made and changed a row is the workspace's to know.
		r.CreatedBy, r.UpdatedBy, r.DeletedBy = nil, nil, nil
	}
	for _, f := range b.Fields {
		f.Config = guestConfig(f.Config)
		// How members set the field up, not what a guest's view shows.
		f.Description, f.Default, f.Required = nil, nil, false
	}
}

// guestConfigKeys are the field settings a guest's view reads (the web's
// GuestTableViewer): how options and numbers show, and what a formula or a
// rollup gives.
var guestConfigKeys = []string{"options", "tinted", "format", "result"}

// guestConfig is a field's config as a guest gets it: guestConfigKeys alone,
// and ai_filled for an AI column instead of its prompt. The rest is how
// members set the field up: another table's id and name, a rollup's fields
// there, a formula's text.
func guestConfig(config string) string {
	var cfg map[string]json.RawMessage
	if json.Unmarshal([]byte(config), &cfg) != nil {
		return "{}"
	}
	out := make(map[string]json.RawMessage, len(guestConfigKeys)+1)
	for _, k := range guestConfigKeys {
		if v, ok := cfg[k]; ok {
			out[k] = v
		}
	}
	if v, ok := cfg["ai"]; ok && string(v) != "null" {
		out["ai_filled"] = json.RawMessage("true")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ───────────────────────── fields ─────────────────────────

// CreateField adds a column (manage only).
func CreateField(ctx context.Context, tableId uuid.UUID, in FieldInput, actor Actor) (*model.Field, error) {
	t, err := loadManageable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	other, err := computedFieldConfig(ctx, actor, t, fields, uuid.Nil, &in)
	if err != nil {
		return nil, err
	}
	f, _, err := buildField(tableId, in, nil, false)
	if err != nil {
		return nil, err
	}
	if other != nil {
		f, err = createLinkPair(ctx, f, t, other)
	} else if f.Id, err = model.CreateField(ctx, f); err != nil {
		err = fmt.Errorf("failed to create field")
	}
	if err != nil {
		return nil, err
	}
	helpers.GoSafeNamed("tellTablesLinkedWith", func() { tellTablesLinkedWith(context.WithoutCancel(ctx), tableId) })
	return f, nil
}

// UpdateField edits a column (manage only): what the patch names changes,
// and the rest keeps its stored value (FieldPatch.onto). Its cells change
// with it, in the same transaction: converted to a new type
// (convertCells.go), and with its options (selectOptions.go), as planned
// against the field as that transaction finds it; a save of a whole field is
// read as v2.72.2 read it (FieldPatch.whole). It returns how many rows' cells
// changed, and has the table's open views read it again.
func UpdateField(ctx context.Context, tableId, fieldId uuid.UUID, p FieldPatch, actor Actor) (int, error) {
	t, err := loadManageable(ctx, tableId, actor)
	if err != nil {
		return 0, err
	}
	fields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return 0, fmt.Errorf("failed to load fields")
	}
	var was *model.Field
	for _, f := range fields {
		if f.Id == fieldId {
			was = f
		}
	}
	if was == nil {
		return 0, errFieldNotFound
	}
	signs, ok := p.NumberSigns.Checked()
	if !ok {
		return 0, errNumberSigns
	}
	if p.Primary != nil {
		if err := primaryProblem(was, *p.Primary, fields); err != nil {
			return 0, err
		}
	}
	in := p.onto(was)
	other, err := computedFieldConfig(ctx, actor, t, fields, fieldId, &in)
	if err != nil {
		return 0, err
	}
	if p.Config != nil {
		p.Config = in.Config // as checked: a formula's fields named by id
	}
	whole := p.whole()
	f, _, err := buildField(tableId, in, was, whole)
	if err != nil {
		return 0, err
	}
	f.Id = fieldId
	changed := 0
	var cs *model.ChangeSet
	if other != nil {
		err = showLinksIn(ctx, f, t, other)
	} else {
		var planErr error
		// The cells the change rewrites are a change set of their own (the
		// row ledger): each row's history says the field's change changed it.
		cs = newChangeSet(ctx, tableId, actor, rowLedger.ChangeField)
		changed, err = model.UpdateFieldAndCells(ctx, tableId, fieldId, func(current *model.Field) (*model.Field, model.Convert, []model.CellRewrite, error) {
			if current.Type != was.Type {
				planErr = errFieldChanged
				return nil, nil, nil, planErr
			}
			f, relabel, err := buildField(tableId, p.onto(current), current, whole)
			if err != nil {
				planErr = err
				return nil, nil, nil, err
			}
			f.Id = fieldId
			var convert model.Convert
			if converts(current.Type, f.Type) && !whole {
				convert = func(cells map[uuid.UUID]json.RawMessage) (map[uuid.UUID]json.RawMessage, error) {
					out, _, err := conversion(f, cells, signs)
					return out, err
				}
			}
			return f, convert, model.Relabel(relabel), nil
		}, cs, observe)
		switch {
		case planErr != nil:
			err = planErr
		case errors.Is(err, sql.ErrNoRows):
			err = errFieldNotFound
		case err != nil:
			err = fmt.Errorf("failed to update field")
		}
	}
	if err != nil {
		return 0, err
	}
	if p.Primary != nil && *p.Primary && !was.Primary {
		if err := model.SetPrimary(ctx, tableId, fieldId); err != nil {
			return 0, fmt.Errorf("failed to make it the primary field")
		}
	}
	tellFieldsChanged(tableId)
	go tellTablesLinkedWith(context.WithoutCancel(ctx), tableId)
	announce(ctx, t, cs, hooksOnly)
	return changed, nil
}

// errPrimaryKept is a field asked to stop being the primary: one becomes the
// primary instead, so a table always has a field naming its rows.
var errPrimaryKept = fmt.Errorf("Make another field the primary field instead")

// primaryProblem is why field f can't be (primary) or stop being the
// primary field: only another becoming it moves the primary, and a field
// whose cells name no row (a link, a rollup, a formula reading either) can't
// name a table's rows.
func primaryProblem(f *model.Field, primary bool, fields []*model.Field) error {
	if !primary {
		if f.Primary {
			return errPrimaryKept
		}
		return nil
	}
	if !namesRows(f, fields) {
		return fmt.Errorf("%q can't be the primary field: a link or a rollup can't name a table's rows", f.Name)
	}
	return nil
}

// tellFieldsChanged has a table's open views read it again: a field changed,
// and perhaps its cells, and a view going on with the field as it was (an
// option's old label) would write what the field no longer offers.
func tellFieldsChanged(tableID uuid.UUID) {
	mqttBusiness.PublishTableRow(tableID.String(), map[string]interface{}{"action": "fields"})
}

// errPrimaryDeleted is a delete of the primary field: the field naming the
// table's rows, which a table always has.
var errPrimaryDeleted = fmt.Errorf("This is the primary field, which names every row: make another field the primary first")

// DeleteField removes a column (manage only).
func DeleteField(ctx context.Context, tableId, fieldId uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, tableId, actor); err != nil {
		return err
	}
	fields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return fmt.Errorf("failed to load fields")
	}
	var field *model.Field
	for _, f := range fields {
		if f.Id == fieldId {
			field = f
		}
	}
	if field != nil && field.Primary {
		return errPrimaryDeleted
	}
	// The tables told are those linking with this one before the field goes.
	linked, _ := model.TablesLinkedWith(ctx, tableId)
	if field != nil {
		// A relation linking to a table goes with the field showing its links
		// there.
		err = unlinkPair(ctx, field, actor.UserID)
	} else {
		err = model.DeleteField(ctx, tableId, fieldId, actor.UserID)
	}
	if err == nil {
		tellTables(linked...)
	}
	return err
}

// buildField checks a field being saved, and gives a select field's options
// their ids against those of the field as it is (was; nil for a new one), the
// options listed being all it has when the save is of a whole field (whole).
// It returns the field, and how its cells change with its options
// (planOptions). It plans on a copy of in's config, so it gives the same
// answer however often it's asked: once to check a save, and again in its
// transaction.
func buildField(tableId uuid.UUID, in FieldInput, was *model.Field, whole bool) (*model.Field, map[string]string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, nil, fmt.Errorf("field name is required")
	}
	if tooLong(name, maxNameLen) {
		return nil, nil, fmt.Errorf("field name is too long")
	}
	ftype := strings.TrimSpace(in.Type)
	if ftype == "" {
		ftype = model.FieldText
	}
	if !fieldTypes.Kinds.Valid(ftype) {
		return nil, nil, fmt.Errorf("invalid field type")
	}
	description := strings.TrimSpace(in.Description)
	if tooLong(description, maxDescriptionLen) {
		return nil, nil, fmt.Errorf("field description is too long")
	}
	sent, err := jsonObject(in.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid field config")
	}
	cfg := map[string]interface{}{}
	_ = json.Unmarshal([]byte(sent), &cfg)
	removed := removedOptions(cfg)
	delete(cfg, removedOptionsKey)
	var relabel map[string]string
	if hasOptions(ftype) {
		var stored []map[string]interface{}
		if was != nil && hasOptions(was.Type) {
			var old map[string]interface{}
			_ = json.Unmarshal([]byte(was.Config), &old)
			stored = fieldTypes.Options(old)
		}
		relabel = planOptions(stored, cfg, removed, whole)
	}
	out, err := jsonObject(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid field config")
	}
	f := &model.Field{TableId: tableId, Name: name, Type: ftype, Config: out, Position: in.Position, Required: in.Required}
	if description != "" {
		f.Description = &description
	}
	if f.Default, err = defaultFor(f, in.Default); err != nil {
		if !in.keptDefault {
			return nil, nil, err
		}
		f.Default = nil
	}
	return f, relabel, nil
}

// defaultFor is a default value as field f stores it (nil for none), or why
// f can't start new rows with it: what its kind would make of the value, as a
// write of it would store it. A field changing type keeps a default the new
// kind can read, and loses one it can't.
func defaultFor(f *model.Field, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid default value")
	}
	stored, err := fieldTypes.Kinds.CheckDefault(f, v)
	if err != nil || stored == nil {
		return nil, err
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("invalid default value")
	}
	if len(b) > maxValuesBytes/10 {
		return nil, fmt.Errorf("default value is too long")
	}
	return b, nil
}

// ───────────────────────── rows ─────────────────────────

// CreateRow inserts a row (view/edit access) and broadcasts it. A row with no
// position goes after the table's last row, wherever that is; the web put a
// new row after the last one it held, which on a table past one page sorted it
// outside every page it held, and it never appeared.
func CreateRow(ctx context.Context, tableId uuid.UUID, in RowInput, actor Actor) (*model.Row, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, ferr := model.ListFields(ctx, tableId)
	if ferr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	values := in.Values
	if values == nil {
		values = map[string]interface{}{}
	}
	id := uuid.New()
	links, linkedTables, lerr := takeLinks(ctx, actor, fields, values, id)
	if lerr != nil {
		return nil, lerr
	}
	valuesJSON, verr := storedCells(fields, values, fieldTypes.Input{New: true, Complete: completeWrite(actor)})
	if verr != nil {
		return nil, verr
	}
	cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeCreate)
	done, cerr := model.ApplyBatch(ctx, tableId, actor.UserID, model.Batch{
		Create: []model.NewRow{{ID: id, Values: valuesJSON}}, At: model.Place{Number: in.Position}, Links: links,
	}, nil, maxValuesBytes, maxLinks, keys, cs, observe)
	if errors.Is(cerr, model.ErrTooManyLinks) {
		return nil, errTooManyLinks
	}
	if cerr != nil {
		return nil, fmt.Errorf("failed to create row")
	}
	created := done.Created[0]
	written := changedCells("{}", created.Values)
	stored := *created // as written: what its AI columns read
	withComputed(ctx, fields, []*model.Row{created})
	broadcastRow(t.Id.String(), "created", created)
	tellTables(linkedTables...)
	announce(ctx, t, cs, hooksOnly)
	// Continuous AI autofill: fill the new row's auto AI columns it wasn't
	// given, as the creator, off the request path. Loop-safe (writes via the
	// model layer, not CreateRow/UpdateRow).
	go AutofillRowOnWrite(context.WithoutCancel(ctx), t, &stored, written, actor)
	// Reactive triggers: let agents react to new table rows (Notion-DB-style
	// automation). Loop-safe — DispatchEvent skips in-process listeners for
	// automation-generated writes, so an agent's own row write can't re-trigger.
	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "table.row.created", map[string]interface{}{
			"table_id":   t.Id.String(),
			"table_name": t.Name,
			"row_id":     created.Id.String(),
		})
	})
	return created, nil
}

// UpdateRow replaces a row's values (view/edit access) and broadcasts it. A
// row given no position stays where it is; a row that's gone is
// errRowNotFound.
func UpdateRow(ctx context.Context, tableId, rowId uuid.UUID, in RowInput, actor Actor) (*model.Row, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, ferr := model.ListFields(ctx, tableId)
	if ferr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	values := in.Values
	if values == nil {
		values = map[string]interface{}{}
	}
	// Its links stay as they are: a row written back as an earlier read gave
	// it would otherwise put back links removed since, or take away those
	// added. They change through ChangeLinks. Only the cells it changes are
	// checked: the web released before patches sends a cell edit as its copy
	// of the whole row, and a cell it didn't touch, stored before the field
	// types checked writes, mustn't make the edit fail.
	stored, rerr := model.GetRowByID(ctx, tableId, rowId)
	if rerr != nil {
		return nil, fmt.Errorf("failed to update row")
	}
	if stored == nil {
		return nil, errRowNotFound
	}
	valuesJSON, verr := storedCells(fields, values, fieldTypes.Input{Unchanged: parseRowValues(stored.Values)})
	if verr != nil {
		return nil, verr
	}
	cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeEdit)
	r, uerr := model.WriteRow(ctx, tableId, rowId, actor.UserID, valuesJSON, in.Position, aiColumns(fields), maxValuesBytes, keys, cs, observe)
	if errors.Is(uerr, model.ErrRowTooLarge) {
		return nil, fmt.Errorf("row is too large")
	}
	if errors.Is(uerr, sql.ErrNoRows) {
		return nil, errRowNotFound
	}
	if uerr != nil {
		return nil, fmt.Errorf("failed to update row")
	}
	return rowUpdated(ctx, t, fields, r, cs, actor), nil
}

// PatchRow changes only the cells it is given, leaving the row's other cells
// and its place as they are when it's written: what a cell edit, a card moved
// on the board and an agent asking to set a field mean. A whole row sent from
// a copy read earlier put back every cell changed since, and update_table_row
// used to move the row to the top too. A nil value clears its cell; a row
// that's gone is errRowNotFound; a patch naming no cell the row stores
// (computed cells and links aren't) is errNoCells.
func PatchRow(ctx context.Context, tableId, rowId uuid.UUID, values map[string]interface{}, actor Actor) (*model.Row, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, ferr := model.ListFields(ctx, tableId)
	if ferr != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if values == nil {
		values = map[string]interface{}{}
	}
	// Links change through ChangeLinks, as for UpdateRow.
	patchJSON, verr := storedCells(fields, values, fieldTypes.Input{})
	if verr != nil {
		return nil, verr
	}
	if len(values) == 0 {
		return nil, errNoCells
	}
	cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeEdit)
	done, uerr := model.ApplyBatch(ctx, tableId, actor.UserID, model.Batch{Patch: []model.CellsPatch{{Row: rowId, Cells: patchJSON}}},
		aiColumns(fields), maxValuesBytes, maxLinks, keys, cs, observe)
	if uerr != nil {
		return nil, singleRowErr(uerr, "failed to update row")
	}
	return rowUpdated(ctx, t, fields, done.Patched[0], cs, actor), nil
}

// singleRowErr is what a write of one row says when it's refused: the row is
// gone, too large, or why not.
func singleRowErr(err error, otherwise string) error {
	var re model.RowErrors
	if errors.As(err, &re) && len(re) == 1 {
		err = re[0].Err
	}
	switch {
	case errors.Is(err, model.ErrRowTooLarge):
		return fmt.Errorf("row is too large")
	case errors.Is(err, sql.ErrNoRows):
		return errRowNotFound
	}
	return fmt.Errorf("%s", otherwise)
}

// rowUpdated is what follows a change to a row's cells (cs, its change set):
// tables linking to it recount, its computed cells are worked out, open views
// hear of it, its AI columns that read a changed cell refill, and webhooks
// fire, today's table.row.updated and the ledger's table.rows.changed.
func rowUpdated(ctx context.Context, t *model.DataTable, fields []*model.Field, r *model.Row, cs *model.ChangeSet, actor Actor) *model.Row {
	changed := changedIn(cs, r.Id)
	announce(ctx, t, cs, hooksOnly)
	go tellLinkedTables(context.WithoutCancel(ctx), t.Id, r.Id, false)
	// The row as written, before its cells are worked out for this reader:
	// what its AI columns read, and write only while it's so.
	stored := *r
	withComputed(ctx, fields, []*model.Row{r})
	broadcastRow(t.Id.String(), "updated", r)
	// Continuous AI autofill: refill the auto AI columns that read a changed
	// cell. Loop-safe (model-layer write, no event).
	go AutofillRowOnWrite(context.WithoutCancel(ctx), t, &stored, changed, actor)
	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "table.row.updated", map[string]interface{}{
			"table_id":   t.Id.String(),
			"table_name": t.Name,
			"row_id":     r.Id.String(),
		})
	})
	return r
}

// DeleteRow soft-deletes a row (view/edit access) and broadcasts it: it goes
// to the table's trash, with its cells and place, and its links kept in its
// change set for a restore. A row that's gone is errRowNotFound.
func DeleteRow(ctx context.Context, tableId, rowId uuid.UUID, actor Actor) error {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return err
	}
	cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeDelete)
	done, err := model.ApplyBatch(ctx, tableId, actor.UserID, model.Batch{Delete: []uuid.UUID{rowId}}, nil, maxValuesBytes, maxLinks, keys, cs, observe)
	if err != nil {
		return singleRowErr(err, "failed to delete row")
	}
	mqttBusiness.PublishTableRow(t.Id.String(), map[string]interface{}{"action": "deleted", "row_id": rowId.String()})
	announce(ctx, t, cs, hooksOnly)
	if done.Linked {
		go tellLinkedTables(context.WithoutCancel(ctx), tableId, rowId, true)
	}
	return nil
}

// ListRows returns a page of rows for a table the actor may view.
func ListRows(ctx context.Context, tableId uuid.UUID, actor Actor, limit, offset int) ([]*model.Row, error) {
	ctx = asViewer(ctx, actor)
	if _, err := loadViewable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	rows, err := model.ListRows(ctx, tableId, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable ListRows: %v", err)
		return nil, errRowsUnavailable
	}
	return readWithComputed(ctx, tableId, rows)
}

// storedCells is a row's values as a write stores them, encoded: without what
// each read works out (stripComputed), each cell as its field's kind stores it
// (business/FieldTypes: a number as a number, an option by its label, a link
// to an item as its {id, type} alone; a new row's defaults; a complete write's
// required fields), within the size cap. Every row write of every writer
// passes here: the web, the API, AI tools, imports, AI generation, Turn into
// database.
func storedCells(fields []*model.Field, values map[string]interface{}, in fieldTypes.Input) (string, error) {
	delete(values, model.AIKeptKey) // set by the server alone
	checked, err := fieldTypes.Kinds.Check(fields, stripComputed(fields, values), in)
	if err != nil {
		return "", err
	}
	return validateValues(liveCells(fields, checked))
}

// liveCells is cells without those of fields that aren't among fields (the
// table's live ones): a field in the trash keeps its cells as stored for its
// restore, so no write names them, and a field that's gone, or never was, has
// none to write.
func liveCells(fields []*model.Field, cells map[string]interface{}) map[string]interface{} {
	live := make(map[string]bool, len(fields))
	for _, f := range fields {
		live[f.Id.String()] = true
	}
	for k := range cells {
		if !live[k] {
			delete(cells, k)
		}
	}
	return cells
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

// broadcastRow tells a table's live topic a row was created or updated, by
// id: readers fetch it again, each as they may see it. The row itself isn't
// sent, as its links to other tables were worked out for its writer.
func broadcastRow(tableID, action string, r *model.Row) {
	if r == nil {
		return
	}
	mqttBusiness.PublishTableRow(tableID, map[string]interface{}{"action": action, "row_id": r.Id.String()})
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
	if tooLong(name, maxNameLen) {
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
