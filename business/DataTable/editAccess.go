package business

// Who edits a table's rows (V-170; plan release-1011/logs/tables-118/v170-plan.md). The permission model:
//   - its owner (creator) and admins open it, edit its rows and manage it;
//   - a member, on a workspace table, opens it and comments; they edit its rows
//     when its general level is edit (every table's until its owner chooses),
//     or when granted edit; on a private table, nothing; a guest link reads;
//   - the links between two tables' rows change for whoever edits the rows of
//     the table they're changed from and opens the other (ChangeLinks).
// An agent edits only what the person who asked it may too (AlsoFor); an API
// token acts as its member. Every row write asks loadEditable; reads keep
// loadViewable (editAccessGuard_test.go holds that).

import (
	"context"
	"errors"
	"fmt"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ReadOnlyWords is what a refused write by someone who may only view a table
// says, in the API and on the web.
const ReadOnlyWords = "You can view this table"

// errReadOnly is a write by someone who may open the table but not edit its
// rows. It wraps errForbidden, so every caller that refuses on IsForbidden
// still refuses; IsReadOnly gives the words (ReadOnlyWords).
var errReadOnly = fmt.Errorf("you can view this table: %w", errForbidden)

// IsReadOnly is a write refused because its actor may only view the table.
func IsReadOnly(err error) bool { return errors.Is(err, errReadOnly) }

// rowsEditable is whether a, who may open t, edits its rows: its managers
// always, its members when its general level is edit or when chosen (granted
// edit). Pure: chosen is the caller's one grant read. A level the database
// didn't fill ("") edits nothing.
func rowsEditable(t *model.DataTable, a Actor, chosen bool) bool {
	if t == nil || !canView(t, a) {
		return false
	}
	return canManage(t, a) || t.GeneralLevel == model.GeneralEdit || chosen
}

// needsGrant is whether a's edit on t turns on a grant: a signed-in member's,
// on a table they open whose general level isn't edit.
func needsGrant(t *model.DataTable, a Actor) bool {
	return a.UserID != uuid.Nil && canView(t, a) && !canManage(t, a) && t.GeneralLevel != model.GeneralEdit
}

// edits is whether a edits t's rows, reading a's grant only when it matters.
func edits(ctx context.Context, t *model.DataTable, a Actor) (bool, error) {
	chosen := false
	if needsGrant(t, a) {
		var err error
		if chosen, err = model.EditGranted(ctx, t.Id, a.UserID); err != nil {
			return false, err
		}
	}
	return rowsEditable(t, a, chosen), nil
}

// editsRows is whether a, and the second person ctx names (AlsoFor), both
// edit t's rows: an agent asked by someone edits only what both of them may.
func editsRows(ctx context.Context, t *model.DataTable, a Actor) (bool, error) {
	ok, err := edits(ctx, t, a)
	if err != nil || !ok {
		return false, err
	}
	if also, has := ctx.Value(alsoKey{}).(Actor); has {
		return edits(ctx, t, also)
	}
	return true, nil
}

// loadEditable loads a table whose rows the actor, and AlsoFor's second
// person, may change: loadViewable's answers, and errReadOnly for someone who
// may only view it.
func loadEditable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataTable, error) {
	t, err := loadViewable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	ok, err := editsRows(ctx, t, actor)
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	if !ok {
		return nil, errReadOnly
	}
	return t, nil
}

// loadEditables is loadEditable for many tables at once: one read of the
// tables, then one read of the grants that matter for each person asked
// (decideTables). It returns the tables in the order of ids, or the first
// refusal in loadEditable's words: errNotFound for a table that isn't there,
// errForbidden for one the actor or AlsoFor's second person can't open, then
// errReadOnly when either of them may only view one.
func loadEditables(ctx context.Context, ids []uuid.UUID, actor Actor) ([]*model.DataTable, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := model.TablesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	live := make(map[uuid.UUID]*model.DataTable, len(found))
	for _, t := range found {
		if t.DeletedAt == nil {
			live[t.Id] = t
		}
	}
	out := make([]*model.DataTable, 0, len(ids))
	for _, id := range ids {
		t := live[id]
		if t == nil {
			return nil, errNotFound
		}
		if !opens(ctx, t, actor) {
			return nil, errForbidden
		}
		out = append(out, t)
	}
	if err := allEditable(ctx, out, actor); err != nil {
		return nil, err
	}
	if also, ok := ctx.Value(alsoKey{}).(Actor); ok {
		if err := allEditable(ctx, out, also); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// allEditable is errReadOnly unless a edits the rows of every one of tables,
// asked in one read of a's grants.
func allEditable(ctx context.Context, tables []*model.DataTable, a Actor) error {
	decided, err := decideTables(ctx, tables, a)
	if err != nil {
		return fmt.Errorf("failed to load table")
	}
	for _, t := range tables {
		if decided[t.Id].Level < accessBusiness.LevelEdit {
			return errReadOnly
		}
	}
	return nil
}

// decideTables is the Access Resolver's decision on each of tables for a:
// tableDecision's, with ONE read of a's grants for every table whose rows a
// edits only when chosen. The one decider behind the "table" and "row" kinds.
func decideTables(ctx context.Context, tables []*model.DataTable, a Actor) (map[uuid.UUID]accessBusiness.Decision, error) {
	var asks []uuid.UUID
	for _, t := range tables {
		if needsGrant(t, a) {
			asks = append(asks, t.Id)
		}
	}
	chosen, err := model.EditGrantedAmong(ctx, a.UserID, asks)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]accessBusiness.Decision, len(tables))
	for _, t := range tables {
		out[t.Id] = tableDecision(t, a, chosen[t.Id])
	}
	return out, nil
}

// withEditing says on a member's bundle whether its reader edits the table's
// rows and, for its managers, who does.
func withEditing(ctx context.Context, b *TableBundle, actor Actor) error {
	ok, err := editsRows(ctx, b.Table, actor)
	if err != nil {
		return fmt.Errorf("failed to load table")
	}
	b.CanEdit = ok
	if !b.CanManage {
		return nil
	}
	access, err := model.EditAccessOf(ctx, b.Table.Id)
	if err != nil {
		return fmt.Errorf("failed to load table")
	}
	b.Editing = &access
	return nil
}

// editingRefusal is a change to who edits a table that can't be made, in the
// words its owner reads: the Share dialog's Who can edit shows them as they
// are (the 400's msg), so they're whole sentences, kept in a type of their
// own as Calendar's EventInputError keeps its, never in an error string.
type editingRefusal struct{ words string }

func (e *editingRefusal) Error() string { return e.words }

var (
	// errEditingPrivate is a choice of who edits a private table, which only
	// its owner and admins open.
	errEditingPrivate = &editingRefusal{"Only you and admins open a private table, so there's no one to choose. Share it with the workspace first."}
	// errEditingLevel is a general level a table can't take yet: members edit
	// its rows or comment on them (the plain viewer comes with sharing).
	errEditingLevel = &editingRefusal{"Choose whether everyone who can open this table edits it, or only the people you choose."}
	// errEditingNotMembers is a person to choose who isn't a member: guests,
	// bots and removed accounts never edit a table's rows.
	errEditingNotMembers = &editingRefusal{"Only members of the workspace can be chosen to edit a table."}
	// errEditingBoth is a person both added and removed by one change.
	errEditingBoth = &editingRefusal{"A person can't be added and removed in one change."}
	// errEditingTooMany is a change granting more than model.MaxEditors.
	errEditingTooMany = &editingRefusal{fmt.Sprintf("A table can name at most %d people who edit it. Let everyone who can open it edit instead.", model.MaxEditors)}
	// errEditingUnrecorded is a change the audit log couldn't take, so it
	// wasn't made.
	errEditingUnrecorded = &editingRefusal{"That couldn't be recorded in the audit log, so it wasn't changed. Try again."}
)

// IsEditingUnrecorded is errEditingUnrecorded: a 503 to try again.
func IsEditingUnrecorded(err error) bool { return errors.Is(err, errEditingUnrecorded) }

// EditingChange is a manager's change to who edits a table's rows: its
// general level ("edit" or "comment"), people to grant edit and people whose
// grants end.
type EditingChange struct {
	General *string     `json:"general,omitempty"`
	Add     []uuid.UUID `json:"add,omitempty"`
	Remove  []uuid.UUID `json:"remove,omitempty"`
}

// GetEditing is who edits a table's rows, for its managers.
func GetEditing(ctx context.Context, id uuid.UUID, actor Actor) (*model.EditAccess, error) {
	if _, err := loadManageable(ctx, id, actor); err != nil {
		return nil, err
	}
	access, err := model.EditAccessOf(ctx, id)
	if errors.Is(err, model.ErrTableGone) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load table")
	}
	return &access, nil
}

// uniqueIDs is ids once each, in their order, without the nil id.
func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != uuid.Nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// checkEditingChange is in, checked against t before anything is stored.
func checkEditingChange(ctx context.Context, t *model.DataTable, in EditingChange) (model.EditChange, error) {
	if t.Visibility != model.VisibilityWorkspace {
		return model.EditChange{}, errEditingPrivate
	}
	if in.General != nil && *in.General != model.GeneralEdit && *in.General != model.GeneralComment {
		return model.EditChange{}, errEditingLevel
	}
	add, remove := uniqueIDs(in.Add), uniqueIDs(in.Remove)
	if len(add) > model.MaxEditors || len(remove) > model.MaxEditors {
		return model.EditChange{}, errEditingTooMany
	}
	removing := make(map[uuid.UUID]bool, len(remove))
	for _, id := range remove {
		removing[id] = true
	}
	for _, id := range add {
		if removing[id] {
			return model.EditChange{}, errEditingBoth
		}
	}
	members, err := userModel.MembersAmong(ctx, add)
	if err != nil {
		return model.EditChange{}, fmt.Errorf("failed to check who to add")
	}
	for _, id := range add {
		if !members[id] {
			return model.EditChange{}, errEditingNotMembers
		}
	}
	return model.EditChange{General: in.General, Add: add, Remove: remove}, nil
}

// editingAudit is the audit entry for a change to who edits t: what it was
// before and after, and who was added and removed.
func editingAudit(t *model.DataTable, before, after model.EditAccess) map[string]interface{} {
	was := make(map[uuid.UUID]bool, len(before.Editors))
	for _, e := range before.Editors {
		was[e.UserID] = true
	}
	now := make(map[uuid.UUID]bool, len(after.Editors))
	added := []string{}
	for _, e := range after.Editors {
		now[e.UserID] = true
		if !was[e.UserID] {
			added = append(added, e.UserID.String())
		}
	}
	removed := []string{}
	for _, e := range before.Editors {
		if !now[e.UserID] {
			removed = append(removed, e.UserID.String())
		}
	}
	ids := func(a model.EditAccess) []string {
		out := make([]string, len(a.Editors))
		for i, e := range a.Editors {
			out[i] = e.UserID.String()
		}
		return out
	}
	return map[string]interface{}{
		"table_id":       t.Id.String(),
		"table_name":     t.Name,
		"general_before": before.General,
		"general_after":  after.General,
		"editors_before": ids(before),
		"editors_after":  ids(after),
		"added":          added,
		"removed":        removed,
	}
}

// auditKind is who acted, in the audit log's words: an agent, or a person.
func auditKind(a Actor) string {
	if a.Kind == rowLedger.ActorAgent {
		return auditBusiness.ActorAgent
	}
	return auditBusiness.ActorHuman
}

// SetEditing makes a manager's change to who edits a table's rows. A change
// that changes something is written to the audit log (CategoryTables) inside
// its transaction, so it never happens unrecorded, and open pages of the
// table are told to read their level again. It returns who edits the table
// now, and whether anything changed.
func SetEditing(ctx context.Context, id uuid.UUID, in EditingChange, actor Actor) (*model.EditAccess, bool, error) {
	t, err := loadManageable(ctx, id, actor)
	if err != nil {
		return nil, false, err
	}
	change, err := checkEditingChange(ctx, t, in)
	if err != nil {
		return nil, false, err
	}
	_, email, _ := auditBusiness.ActorFromContext(ctx)
	recorded := false
	record := func(before, after model.EditAccess) error {
		if err := auditBusiness.RecordForPrincipal(ctx, &actor.UserID, email, auditKind(actor), "table.editing.changed",
			auditBusiness.CategoryTables, "Changed who edits a table", editingAudit(t, before, after)); err != nil {
			return errEditingUnrecorded
		}
		recorded = true
		return nil
	}
	_, after, err := model.SetEditAccess(ctx, id, actor.UserID, change, record)
	switch {
	case errors.Is(err, errEditingUnrecorded):
		return nil, false, err
	case errors.Is(err, model.ErrTooManyEditors):
		return nil, false, errEditingTooMany
	case errors.Is(err, model.ErrTableGone):
		return nil, false, errNotFound
	case err != nil:
		if recorded {
			// Written down, then not made: the log says so.
			_ = auditBusiness.RecordForPrincipal(ctx, &actor.UserID, email, auditKind(actor), "table.editing.change_failed",
				auditBusiness.CategoryTables, "Changing who edits a table didn't take", map[string]interface{}{"table_id": t.Id.String()})
		}
		return nil, false, fmt.Errorf("failed to change who edits the table")
	}
	if recorded {
		mqttBusiness.PublishTableRow(id.String(), map[string]interface{}{"action": "editing"})
	}
	return &after, recorded, nil
}
